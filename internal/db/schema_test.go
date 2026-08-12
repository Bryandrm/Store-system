package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bryandrm/store-system/internal/testdb"
)

// tablasAppendOnly son las que jamas deben admitir UPDATE ni DELETE desde el
// rol de la aplicacion. La columna se usa solo para armar un UPDATE sintacticamente
// valido: si se usara una columna inexistente, Postgres fallaria por nombre
// invalido ANTES de llegar al chequeo de permisos y el test pasaria en falso.
var tablasAppendOnly = []struct {
	tabla   string
	columna string
}{
	{"sales", "id"},
	{"sale_lines", "id"},
	{"sale_voids", "sale_id"},
	{"stock_movements", "id"},
	{"customer_ledger", "id"},
	{"payments", "id"},
	{"product_prices", "id"},
	{"restocks", "id"},
	{"restock_lines", "id"},
	{"change_log", "seq"},
	{"change_log_floor", "singleton"},
	{"sync_operations", "op_id"},
}

// TestAppendOnlyImpuestoPorPostgres es el test que sostiene la invariante
// central del sistema.
//
// Todo el diseño descansa en que nada se modifica ni se borra. Si eso dependiera
// de que el codigo Go se porte bien, seria una convencion, y las convenciones se
// rompen a las tres features. Aca se verifica que la base directamente NO DEJE.
//
// Si este test falla, no es un detalle de permisos: significa que un UPDATE mal
// puesto puede corromper la contabilidad en silencio.
func TestAppendOnlyImpuestoPorPostgres(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	for _, c := range tablasAppendOnly {
		t.Run(c.tabla, func(t *testing.T) {
			_, err := tdb.App.Exec(ctx,
				"UPDATE "+c.tabla+" SET "+c.columna+" = "+c.columna)
			exigirPermisoDenegado(t, err, "UPDATE sobre "+c.tabla)

			_, err = tdb.App.Exec(ctx, "DELETE FROM "+c.tabla)
			exigirPermisoDenegado(t, err, "DELETE sobre "+c.tabla)
		})
	}
}

// TestCatalogoAdmiteUpdate verifica el otro lado de la linea: la metadata
// cosmetica SI se actualiza por last-write-wins.
//
// Sin este test, "blindar todo" pareceria correcto y romperia el renombre de un
// producto.
func TestCatalogoAdmiteUpdate(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	for _, tabla := range []string{"products", "customers", "users", "sessions"} {
		if _, err := tdb.App.Exec(ctx, "UPDATE "+tabla+" SET created_at = created_at"); err != nil {
			if strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s deberia admitir UPDATE (metadata LWW), pero fue denegado", tabla)
			}
		}
	}
}

// TestAppPuedeInsertarYLeer comprueba que el blindaje no se paso de rosca: la
// aplicacion tiene que poder hacer su trabajo.
func TestAppPuedeInsertarYLeer(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	var uno int
	if err := tdb.App.QueryRow(ctx, "SELECT 1 FROM sales LIMIT 1").Scan(&uno); err != nil {
		if strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("la aplicacion no puede leer sales: %v", err)
		}
	}

	// change_log.seq es BIGSERIAL: sin USAGE sobre la secuencia, todo INSERT
	// falla. Es un permiso facil de olvidar y rompe el sistema entero.
	_, err := tdb.App.Exec(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ('prueba', gen_random_uuid(), 'insert', '{}'::jsonb)`)
	if err != nil {
		t.Fatalf("la aplicacion no puede escribir en change_log: %v", err)
	}
}

// TestHistorialDeMigracionesProtegido: la aplicacion no debe poder falsificar
// el historial de migraciones.
func TestHistorialDeMigracionesProtegido(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	var privilegios int
	err := tdb.Admin.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		 WHERE grantee = 'store_app' AND table_name = 'goose_db_version'`).Scan(&privilegios)
	if err != nil {
		t.Fatalf("no se pudieron leer los permisos: %v", err)
	}
	if privilegios != 0 {
		t.Errorf("store_app tiene %d permisos sobre goose_db_version; no deberia tener ninguno", privilegios)
	}
}

// TestVistasDerivadasResponden verifica que el stock y los saldos se derivan y
// que la aplicacion puede consultarlos.
func TestVistasDerivadasResponden(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	for _, vista := range []string{"stock_levels", "customer_balances", "current_prices"} {
		var n int
		if err := tdb.App.QueryRow(ctx, "SELECT count(*) FROM "+vista).Scan(&n); err != nil {
			t.Errorf("no se pudo consultar la vista %s: %v", vista, err)
		}
	}
}

func exigirPermisoDenegado(t *testing.T, err error, operacion string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s fue PERMITIDO; el blindaje append-only no esta activo", operacion)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("%s fallo por otra razon (el test puede estar mal escrito): %v", operacion, err)
	}
}
