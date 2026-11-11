package orders

import "fmt"

// OrderNumber turns the stored order number into the short form people read out: a letter and four
// digits, such as A2931. The digits are the number modulo 10000, zero padded. The letter steps on
// each time the digits wrap, A for 0-9999, B for 10000-19999, and back to A after Z. So the form
// repeats only after 260000 orders, far longer than an order's life on the road.
func OrderNumber(n int64) string {
	if n < 0 {
		n = -n
	}
	return fmt.Sprintf("%c%04d", 'A'+rune((n/10000)%26), n%10000)
}
