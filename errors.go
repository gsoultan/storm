package raorm

import (
	"errors"
	"sort"
	"strings"
)

// errorList accumulates every declaration problem so Build can report all of
// them at once. Failing on the first error would mean N build cycles to find N
// mistakes.
type errorList struct{ errs []error }

func (l *errorList) add(err error) {
	if err != nil {
		l.errs = append(l.errs, err)
	}
}

func (l *errorList) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	msgs := make([]string, len(l.errs))
	for i, e := range l.errs {
		msgs[i] = "  " + e.Error()
	}
	sort.Strings(msgs)
	return errors.New("raorm: " + plural(len(msgs)) + " in model declarations:\n" + strings.Join(msgs, "\n"))
}

func plural(n int) string {
	if n == 1 {
		return "1 problem"
	}
	return itoa(n) + " problems"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
