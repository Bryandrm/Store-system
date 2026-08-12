// Package money concentra toda la aritmetica de dinero y cantidades.
//
// Reglas que no se negocian:
//   - El dinero es int64 en centavos. Nunca float, nunca NUMERIC.
//   - Las cantidades son int64 en milesimas de unidad (qty_milli).
//   - Redondeo half-up, una sola implementacion, espejada en src/domain/money.ts
//     contra el mismo fixture testdata/money_cases.json.
//
// Si esta implementacion y la de TypeScript se separan, el cliente le dice al
// comprador un total y el servidor guarda otro. Por eso el fixture es
// compartido: es lo unico que garantiza que ambas digan lo mismo.
package money

import "errors"

var (
	// ErrOverflow indica que la operacion excede el rango de int64. Con precios
	// y cantidades reales es inalcanzable; se detecta igual porque el modo de
	// falla silencioso (envolver el signo) seria un desastre contable.
	ErrOverflow = errors.New("money: desbordamiento en aritmetica de enteros")

	// ErrNonPositiveDenominator protege contra division por cero.
	ErrNonPositiveDenominator = errors.New("money: el denominador debe ser positivo")

	// ErrNegative senala un valor negativo donde el dominio no lo admite.
	ErrNegative = errors.New("money: valor negativo no admitido")
)

// QtyScale es la escala de las cantidades: 1000 milesimas = 1 unidad.
//
// Hoy todo se vende por unidad, pero el esquema guarda milesimas a proposito:
// el dia que aparezca "media libra", un qty entero obligaria a migrar clientes
// que tienen datos offline. Cuesta cero ahora.
const QtyScale int64 = 1000

// RoundHalfUp divide numer entre denom redondeando los empates hacia arriba.
//
// Se calcula con cociente y resto en vez de (numer + denom/2) / denom porque
// esa forma solo es exacta con denominadores pares y, con numeradores grandes,
// puede desbordar antes de dividir.
func RoundHalfUp(numer, denom int64) (int64, error) {
	if denom <= 0 {
		return 0, ErrNonPositiveDenominator
	}
	if numer < 0 {
		return 0, ErrNegative
	}

	quot := numer / denom
	rem := numer % denom

	// Empate exacto (rem == denom/2) redondea hacia arriba. Se compara
	// 2*rem >= denom para no depender de la paridad de denom.
	if rem > 0 && 2*rem >= denom {
		quot++
	}
	return quot, nil
}

// LineTotal calcula el total de una linea de venta a partir del precio
// unitario en centavos y la cantidad en milesimas.
//
// Es la funcion que produce el numero que el cliente ve y paga.
func LineTotal(unitPriceCents, qtyMilli int64) (int64, error) {
	if unitPriceCents < 0 || qtyMilli < 0 {
		return 0, ErrNegative
	}
	if unitPriceCents == 0 || qtyMilli == 0 {
		return 0, nil
	}

	// Deteccion de desbordamiento antes de multiplicar.
	if unitPriceCents > (1<<63-1)/qtyMilli {
		return 0, ErrOverflow
	}

	return RoundHalfUp(unitPriceCents*qtyMilli, QtyScale)
}

// Debt es una deuda pendiente de un cliente, para repartir un pago.
type Debt struct {
	// RefID identifica la venta a credito que origino la deuda.
	RefID string `json:"ref_id"`
	// AmountCents es lo que resta pagar de esa deuda. Siempre positivo.
	AmountCents int64 `json:"amount_cents"`
}

// Allocation es la porcion de un pago aplicada a una deuda concreta.
type Allocation struct {
	RefID       string `json:"ref_id"`
	AmountCents int64  `json:"amount_cents"`
}

// AllocatePayment reparte un pago entre las deudas de un cliente, de la mas
// vieja a la mas nueva, en centavos enteros.
//
// El reparto es greedy y NO proporcional. El proporcional siempre deja un
// centavo suelto que hay que poner en algun lado, y donde sea que se ponga es
// una decision arbitraria imposible de explicarle al cliente. El greedy, en
// cambio, se explica en una frase: se cancela la deuda mas vieja primero.
//
// El sobrante despues de cubrir todas las deudas se devuelve como creditCents:
// es el saldo a favor del cliente. Se cumple siempre, y esta probado como
// propiedad sobre repartos aleatorios:
//
//	sum(allocations) + creditCents == paymentCents
//
// debts debe venir ordenada de la mas vieja a la mas nueva; esta funcion no
// reordena, porque el criterio de antiguedad (occurred_at) vive en la consulta.
func AllocatePayment(paymentCents int64, debts []Debt) (allocations []Allocation, creditCents int64, err error) {
	if paymentCents < 0 {
		return nil, 0, ErrNegative
	}

	remaining := paymentCents
	allocations = make([]Allocation, 0, len(debts))

	for _, d := range debts {
		if d.AmountCents < 0 {
			return nil, 0, ErrNegative
		}
		if remaining == 0 || d.AmountCents == 0 {
			continue
		}

		applied := d.AmountCents
		if remaining < applied {
			applied = remaining
		}

		allocations = append(allocations, Allocation{RefID: d.RefID, AmountCents: applied})
		remaining -= applied
	}

	return allocations, remaining, nil
}

// SumAllocations suma las porciones de un reparto. Existe para que la
// invariante quede verificable en una linea, tanto en tests como en cmd/verify.
func SumAllocations(allocations []Allocation) int64 {
	var total int64
	for _, a := range allocations {
		total += a.AmountCents
	}
	return total
}
