// Package testdb da a cada test una base Postgres real y efimera.
//
// Sin mocks, por decision explicita: mockear la base haria que los tests mas
// valiosos del proyecto (convergencia del feed, idempotencia bajo transacciones
// concurrentes) prueben el mock en vez del sistema.
//
// Como funciona: una vez por corrida se crea store_test_template y se le
// aplican las migraciones. Cada test hace CREATE DATABASE ... TEMPLATE, que
// Postgres resuelve copiando archivos: ~30 ms, aislamiento total.
//
// Lo que NO se hace: envolver cada test en una transaccion y revertir. Seria
// mas rapido, pero volveria imposibles los tests que necesitan transacciones
// reales, separadas y commiteando en paralelo, que son justamente los que
// justifican todo el diseño.
package testdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bryandrm/store-system/internal/db"
)

const (
	templateName = "store_test_template"

	// La contraseña de store_app en tests. En produccion la asigna la infra;
	// aca hace falta porque la migracion crea el rol NOLOGIN a proposito.
	testAppPassword = "test_app_pwd"
)

// defaultAdminURL apunta al Postgres de compose.dev.yml.
const defaultAdminURL = "postgres://store_migrator:dev_only_no_usar_en_produccion@localhost:5433/postgres?sslmode=disable"

var (
	setupOnce sync.Once
	setupErr  error
)

// DB es el par de conexiones que recibe un test.
type DB struct {
	// App conecta como store_app: el MISMO rol que usa la API en produccion.
	//
	// Es deliberado. Si los tests corrieran como superusuario, un GRANT
	// faltante pasaria inadvertido hasta el despliegue, y un UPDATE indebido
	// nunca se detectaria porque el superusuario puede hacerlo.
	App *pgxpool.Pool

	// Admin conecta como store_migrator. Se usa solo para preparar escenarios
	// y para aserciones que necesitan ver mas de lo que la aplicacion ve.
	Admin *pgxpool.Pool

	// Name es el nombre de la base efimera, util al depurar un test.
	Name string

	// AppURL sirve para abrir conexiones sueltas cuando un test necesita
	// transacciones concurrentes de verdad (el caso del feed).
	AppURL string
}

// New crea una base efimera para este test y la borra al terminar.
//
//	func TestAlgo(t *testing.T) {
//	    tdb := testdb.New(t)
//	    // usar tdb.App como si fuera el pool de produccion
//	}
func New(t *testing.T) *DB {
	t.Helper()

	setupOnce.Do(func() { setupErr = ensureTemplate() })
	if setupErr != nil {
		t.Fatalf("no se pudo preparar la base plantilla: %v\n\n"+
			"¿Esta levantado Postgres?  docker compose -f compose.dev.yml up -d", setupErr)
	}

	ctx := context.Background()
	name := fmt.Sprintf("store_test_%d_%d", time.Now().UnixNano()%1_000_000, rand.Uint32())

	admin, err := pgxpool.New(ctx, adminURL())
	if err != nil {
		t.Fatalf("no se pudo conectar como administrador: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateName)); err != nil {
		t.Fatalf("no se pudo crear la base %s: %v", name, err)
	}

	appURL := urlForDatabase(adminURL(), name, "store_app", testAppPassword)
	adminDBURL := urlForDatabase(adminURL(), name, "", "")

	appPool, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("no se pudo abrir el pool de aplicacion: %v", err)
	}
	adminPool, err := pgxpool.New(ctx, adminDBURL)
	if err != nil {
		appPool.Close()
		t.Fatalf("no se pudo abrir el pool de administracion: %v", err)
	}

	t.Cleanup(func() {
		appPool.Close()
		adminPool.Close()

		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		cleaner, err := pgxpool.New(cleanupCtx, adminURL())
		if err != nil {
			t.Logf("no se pudo conectar para limpiar %s: %v", name, err)
			return
		}
		defer cleaner.Close()

		// Cortar conexiones rezagadas: un test que abrio conexiones sueltas y
		// no las cerro impediria el DROP y dejaria basura entre corridas.
		_, _ = cleaner.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()`, name)

		if _, err := cleaner.Exec(cleanupCtx,
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", name)); err != nil {
			t.Logf("no se pudo borrar la base %s: %v", name, err)
		}
	})

	return &DB{App: appPool, Admin: adminPool, Name: name, AppURL: appURL}
}

// ensureTemplate crea la base plantilla y le aplica las migraciones. Corre una
// sola vez por proceso de test.
func ensureTemplate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, adminURL())
	if err != nil {
		return fmt.Errorf("conexion de administracion: %w", err)
	}
	defer admin.Close()

	// Se rehace en cada corrida: una plantilla vieja con un esquema anterior es
	// una fuente de fallos fantasma dificiles de diagnosticar.
	_, _ = admin.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = $1 AND pid <> pg_backend_pid()`, templateName)
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+templateName); err != nil {
		return fmt.Errorf("no se pudo borrar la plantilla anterior: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateName); err != nil {
		return fmt.Errorf("no se pudo crear la plantilla: %w", err)
	}

	templateURL := urlForDatabase(adminURL(), templateName, "", "")
	if err := db.Migrate(ctx, templateURL); err != nil {
		return fmt.Errorf("migraciones sobre la plantilla: %w", err)
	}

	// La migracion crea store_app NOLOGIN a proposito (las credenciales las
	// pone la infraestructura). En tests hay que darle acceso.
	if _, err := admin.Exec(ctx,
		fmt.Sprintf("ALTER ROLE store_app LOGIN PASSWORD '%s'", testAppPassword)); err != nil {
		return fmt.Errorf("no se pudo habilitar store_app: %w", err)
	}

	return nil
}

func adminURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultAdminURL
}

// urlForDatabase reescribe la URL para apuntar a otra base y, opcionalmente,
// con otro usuario.
func urlForDatabase(base, database, user, password string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = "/" + strings.TrimPrefix(database, "/")
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}
