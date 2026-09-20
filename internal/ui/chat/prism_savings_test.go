package chat

import (
	"testing"

	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestPrismSavingsSuffix(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	stylesRef := &sty
	ptr := func(v float64) *float64 { return &v }

	strip := func(s string) string { return ansi.Strip(s) }

	t.Run("prefers hypercredits when both are present", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "◆ 2 Saved", strip(prismSavingsSuffix(stylesRef, ptr(1.5), ptr(0.002))))
	})

	t.Run("rounds hypercredits to whole numbers at 1 and above", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "◆ 2 Saved", strip(prismSavingsSuffix(stylesRef, ptr(2.4), nil)))
		require.Equal(t, "◆ 13 Saved", strip(prismSavingsSuffix(stylesRef, ptr(13), nil)))
	})

	t.Run("keeps a single decimal below one hypercredit", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "◆ 0.5 Saved", strip(prismSavingsSuffix(stylesRef, ptr(0.51), nil)))
		require.Equal(t, "◆ 0.0 Saved", strip(prismSavingsSuffix(stylesRef, ptr(0.00136), nil)))
	})

	t.Run("rounds dollars to two decimal digits", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "• $0.00 Saved", strip(prismSavingsSuffix(stylesRef, nil, ptr(0.002))))
		require.Equal(t, "• $1.24 Saved", strip(prismSavingsSuffix(stylesRef, nil, ptr(1.235))))
	})

	t.Run("hypercredit icon carries the subdued hypercredit color", func(t *testing.T) {
		t.Parallel()
		icon := stylesRef.Messages.SubduedHypercreditIcon.Render(styles.HypercreditIcon)
		require.Contains(t, prismSavingsSuffix(stylesRef, ptr(13), nil), icon)
	})

	t.Run("shows no suffix when unreported", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, prismSavingsSuffix(stylesRef, nil, nil))
	})
}
