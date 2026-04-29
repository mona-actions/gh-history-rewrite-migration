package output

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

// TestNoColorBranches exercises the NO_COLOR fast paths in Info/Success/Warn/
// Error/Table. Without these, those branches are 0% covered because the
// existing tests run with NO_COLOR unset.
func TestNoColorBranches(t *testing.T) {
	prev := viper.GetBool("NO_COLOR")
	viper.Set("NO_COLOR", true)
	t.Cleanup(func() { viper.Set("NO_COLOR", prev) })

	assert.NotPanics(t, func() { Info("info-no-color") })
	assert.NotPanics(t, func() { Success("success-no-color") })
	assert.NotPanics(t, func() { Warn("warn-no-color") })
	assert.NotPanics(t, func() { Error("error-no-color") })
	assert.NotPanics(t, func() {
		Table([]string{"a", "b"}, [][]string{{"1", "2"}, {"3", "4"}})
	})
	// Empty rows still hit the loop guard.
	assert.NotPanics(t, func() {
		Table([]string{"only"}, nil)
	})
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{int64(1024) * 1024 * 1024, "1.0 GiB"},
		{int64(2) * 1024 * 1024 * 1024 * 1024, "2.0 TiB"},
	}
	for _, c := range cases {
		got := HumanBytes(c.in)
		assert.Equal(t, c.want, got, "HumanBytes(%d)", c.in)
	}
}
