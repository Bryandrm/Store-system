package money

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath apunta al fixture COMPARTIDO con src/domain/money.test.ts.
// Si cambias esta ruta, cambiala tambien alla: el valor del fixture es que sea
// el mismo archivo, no que sea parecido.
const fixturePath = "../../testdata/money_cases.json"

type fixture struct {
	RoundHalfUp []struct {
		Nombre string `json:"nombre"`
		Numer  int64  `json:"numer"`
		Denom  int64  `json:"denom"`
		Want   int64  `json:"want"`
	} `json:"round_half_up"`

	LineTotal []struct {
		Nombre         string `json:"nombre"`
		UnitPriceCents int64  `json:"unit_price_cents"`
		QtyMilli       int64  `json:"qty_milli"`
		Want           int64  `json:"want"`
	} `json:"line_total"`

	AllocatePayment []struct {
		Nombre          string       `json:"nombre"`
		PaymentCents    int64        `json:"payment_cents"`
		Debts           []Debt       `json:"debts"`
		WantAllocations []Allocation `json:"want_allocations"`
		WantCreditCents int64        `json:"want_credit_cents"`
	} `json:"allocate_payment"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(fixturePath))
	if err != nil {
		t.Fatalf("no se pudo leer el fixture compartido %s: %v", fixturePath, err)
	}

	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fixture invalido: %v", err)
	}
	return f
}

func TestRoundHalfUp(t *testing.T) {
	f := loadFixture(t)
	if len(f.RoundHalfUp) == 0 {
		t.Fatal("el fixture no tiene casos de round_half_up")
	}

	for _, c := range f.RoundHalfUp {
		t.Run(c.Nombre, func(t *testing.T) {
			got, err := RoundHalfUp(c.Numer, c.Denom)
			if err != nil {
				t.Fatalf("RoundHalfUp(%d, %d) devolvio error: %v", c.Numer, c.Denom, err)
			}
			if got != c.Want {
				t.Errorf("RoundHalfUp(%d, %d) = %d, se esperaba %d", c.Numer, c.Denom, got, c.Want)
			}
		})
	}
}

func TestRoundHalfUpErrores(t *testing.T) {
	if _, err := RoundHalfUp(100, 0); err != ErrNonPositiveDenominator {
		t.Errorf("denominador cero: se esperaba ErrNonPositiveDenominator, se obtuvo %v", err)
	}
	if _, err := RoundHalfUp(100, -5); err != ErrNonPositiveDenominator {
		t.Errorf("denominador negativo: se esperaba ErrNonPositiveDenominator, se obtuvo %v", err)
	}
	if _, err := RoundHalfUp(-1, 1000); err != ErrNegative {
		t.Errorf("numerador negativo: se esperaba ErrNegative, se obtuvo %v", err)
	}
}

func TestLineTotal(t *testing.T) {
	f := loadFixture(t)
	if len(f.LineTotal) == 0 {
		t.Fatal("el fixture no tiene casos de line_total")
	}

	for _, c := range f.LineTotal {
		t.Run(c.Nombre, func(t *testing.T) {
			got, err := LineTotal(c.UnitPriceCents, c.QtyMilli)
			if err != nil {
				t.Fatalf("LineTotal(%d, %d) devolvio error: %v", c.UnitPriceCents, c.QtyMilli, err)
			}
			if got != c.Want {
				t.Errorf("LineTotal(%d, %d) = %d, se esperaba %d",
					c.UnitPriceCents, c.QtyMilli, got, c.Want)
			}
		})
	}
}

func TestLineTotalDesbordamiento(t *testing.T) {
	// Valores imposibles en el dominio, pero el modo de falla silencioso
	// (envolver el signo) seria un desastre contable, asi que se detecta.
	const grande = int64(1) << 40
	if _, err := LineTotal(grande, grande); err != ErrOverflow {
		t.Errorf("se esperaba ErrOverflow, se obtuvo %v", err)
	}
	if _, err := LineTotal(-1, 1000); err != ErrNegative {
		t.Errorf("precio negativo: se esperaba ErrNegative, se obtuvo %v", err)
	}
}

func TestAllocatePayment(t *testing.T) {
	f := loadFixture(t)
	if len(f.AllocatePayment) == 0 {
		t.Fatal("el fixture no tiene casos de allocate_payment")
	}

	for _, c := range f.AllocatePayment {
		t.Run(c.Nombre, func(t *testing.T) {
			gotAllocs, gotCredit, err := AllocatePayment(c.PaymentCents, c.Debts)
			if err != nil {
				t.Fatalf("AllocatePayment devolvio error: %v", err)
			}

			if len(gotAllocs) != len(c.WantAllocations) {
				t.Fatalf("cantidad de asignaciones = %d, se esperaba %d (%+v)",
					len(gotAllocs), len(c.WantAllocations), gotAllocs)
			}
			for i := range gotAllocs {
				if gotAllocs[i] != c.WantAllocations[i] {
					t.Errorf("asignacion %d = %+v, se esperaba %+v",
						i, gotAllocs[i], c.WantAllocations[i])
				}
			}
			if gotCredit != c.WantCreditCents {
				t.Errorf("saldo a favor = %d, se esperaba %d", gotCredit, c.WantCreditCents)
			}
		})
	}
}

// TestAllocatePaymentInvariante es el test de propiedad: sobre repartos
// aleatorios, la suma de lo asignado mas el saldo a favor tiene que dar
// exactamente el pago. Ni un centavo se crea ni se pierde.
//
// Es la propiedad que hace que el reparto greedy sea defendible frente al
// proporcional, que siempre deja un centavo suelto sin dueño claro.
func TestAllocatePaymentInvariante(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) // semilla fija: fallos reproducibles

	for i := 0; i < 100; i++ {
		payment := rng.Int63n(100_000)

		debts := make([]Debt, rng.Intn(8))
		for j := range debts {
			debts[j] = Debt{
				RefID:       string(rune('a' + j)),
				AmountCents: rng.Int63n(30_000),
			}
		}

		allocs, credit, err := AllocatePayment(payment, debts)
		if err != nil {
			t.Fatalf("iteracion %d: error inesperado: %v", i, err)
		}

		if got := SumAllocations(allocs) + credit; got != payment {
			t.Fatalf("iteracion %d: suma(asignaciones)=%d + saldo=%d = %d, pero el pago fue %d",
				i, SumAllocations(allocs), credit, got, payment)
		}

		// Ninguna asignacion puede superar su deuda.
		porRef := make(map[string]int64, len(debts))
		for _, d := range debts {
			porRef[d.RefID] = d.AmountCents
		}
		for _, a := range allocs {
			if a.AmountCents > porRef[a.RefID] {
				t.Fatalf("iteracion %d: se asignaron %d a una deuda de %d",
					i, a.AmountCents, porRef[a.RefID])
			}
			if a.AmountCents <= 0 {
				t.Fatalf("iteracion %d: asignacion no positiva %d", i, a.AmountCents)
			}
		}
	}
}

func TestAllocatePaymentNegativos(t *testing.T) {
	if _, _, err := AllocatePayment(-1, nil); err != ErrNegative {
		t.Errorf("pago negativo: se esperaba ErrNegative, se obtuvo %v", err)
	}
	if _, _, err := AllocatePayment(100, []Debt{{RefID: "a", AmountCents: -5}}); err != ErrNegative {
		t.Errorf("deuda negativa: se esperaba ErrNegative, se obtuvo %v", err)
	}
}
