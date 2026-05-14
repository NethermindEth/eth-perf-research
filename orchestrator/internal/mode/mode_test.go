package mode

import (
	"sync"
	"testing"
)

func TestNewModeControllerStartsInRun(t *testing.T) {
	c := NewModeController()
	if got := c.Get(); got != ModeRun {
		t.Fatalf("initial mode = %v, want ModeRun", got)
	}
}

func TestModeStringLabels(t *testing.T) {
	cases := map[Mode]string{
		ModeRun:      "run",
		ModeThrottle: "throttle",
		ModeDrain:    "drain",
		ModeHalt:     "halt",
		Mode(99):     "unknown",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", m, got, want)
		}
	}
}

func TestValidTransitions(t *testing.T) {
	cases := []struct {
		name string
		do   func(c *ModeController)
		want Mode
	}{
		{
			name: "run to throttle",
			do:   func(c *ModeController) { c.RequestThrottle("test") },
			want: ModeThrottle,
		},
		{
			name: "throttle to run",
			do: func(c *ModeController) {
				c.RequestThrottle("first")
				c.RequestRun("recover")
			},
			want: ModeRun,
		},
		{
			name: "run to drain",
			do:   func(c *ModeController) { c.RequestDrain("shutdown") },
			want: ModeDrain,
		},
		{
			name: "throttle to drain",
			do: func(c *ModeController) {
				c.RequestThrottle("first")
				c.RequestDrain("shutdown")
			},
			want: ModeDrain,
		},
		{
			name: "run to halt",
			do:   func(c *ModeController) { c.RequestHalt("emergency") },
			want: ModeHalt,
		},
		{
			name: "throttle to halt",
			do: func(c *ModeController) {
				c.RequestThrottle("first")
				c.RequestHalt("emergency")
			},
			want: ModeHalt,
		},
		{
			name: "drain to halt",
			do: func(c *ModeController) {
				c.RequestDrain("first")
				c.RequestHalt("emergency")
			},
			want: ModeHalt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewModeController()
			tc.do(c)
			if got := c.Get(); got != tc.want {
				t.Fatalf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInvalidTransitionsIgnored(t *testing.T) {
	cases := []struct {
		name string
		do   func(c *ModeController)
		want Mode
	}{
		{
			name: "run to run is no-op",
			do:   func(c *ModeController) { c.RequestRun("no-op") },
			want: ModeRun,
		},
		{
			name: "drain to run ignored",
			do: func(c *ModeController) {
				c.RequestDrain("first")
				c.RequestRun("attempted recovery")
			},
			want: ModeDrain,
		},
		{
			name: "drain to throttle ignored",
			do: func(c *ModeController) {
				c.RequestDrain("first")
				c.RequestThrottle("attempt")
			},
			want: ModeDrain,
		},
		{
			name: "halt to run ignored",
			do: func(c *ModeController) {
				c.RequestHalt("first")
				c.RequestRun("attempt")
			},
			want: ModeHalt,
		},
		{
			name: "halt to throttle ignored",
			do: func(c *ModeController) {
				c.RequestHalt("first")
				c.RequestThrottle("attempt")
			},
			want: ModeHalt,
		},
		{
			name: "halt to drain ignored",
			do: func(c *ModeController) {
				c.RequestHalt("first")
				c.RequestDrain("attempt")
			},
			want: ModeHalt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewModeController()
			tc.do(c)
			if got := c.Get(); got != tc.want {
				t.Fatalf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConcurrentTransitionsConverge(t *testing.T) {
	c := NewModeController()
	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			switch id % 4 {
			case 0:
				c.RequestThrottle("t")
			case 1:
				c.RequestRun("r")
			case 2:
				c.RequestDrain("d")
			case 3:
				c.RequestHalt("h")
			}
		}(i)
	}
	wg.Wait()

	got := c.Get()
	switch got {
	case ModeRun, ModeThrottle, ModeDrain, ModeHalt:
	default:
		t.Fatalf("converged to invalid mode %d", got)
	}

	c.RequestHalt("finalise")
	if c.Get() != ModeHalt {
		t.Fatalf("after final halt, mode = %v, want ModeHalt", c.Get())
	}
}

func TestHaltDominatesConcurrentWrites(t *testing.T) {
	c := NewModeController()
	c.RequestHalt("first")

	var wg sync.WaitGroup
	wg.Add(32)
	for i := 0; i < 32; i++ {
		go func() {
			defer wg.Done()
			c.RequestRun("attempted")
			c.RequestThrottle("attempted")
			c.RequestDrain("attempted")
		}()
	}
	wg.Wait()

	if c.Get() != ModeHalt {
		t.Fatalf("halt was overwritten, mode = %v", c.Get())
	}
}
