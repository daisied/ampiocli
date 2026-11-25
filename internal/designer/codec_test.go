package designer

import (
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// Designer's code is stopped if it runs too long, and the interpreter is
// usable again afterwards.
func TestLimit(t *testing.T) {
	c := &Codec{vm: goja.New()}
	stop := c.limit(50 * time.Millisecond)
	_, err := c.vm.RunString("for (;;) {}")
	stop()
	if err == nil || !strings.Contains(err.Error(), "still running after 50ms") {
		t.Fatalf("want an interrupt, got %v", err)
	}
	if v, err := c.vm.RunString("1 + 1"); err != nil || v.ToInteger() != 2 {
		t.Errorf("after the interrupt: %v, %v", v, err)
	}
}
