package tui

import (
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
)

const defaultWidth = 80

// borgMarkdownStyle is glamour's dark theme with the literal "##"/"###" heading
// prefixes removed — the stock dark style renders `### Foo` as "### Foo" (just
// bold-blue), which reads as broken markdown. Headings keep their bold-blue
// color; H2/H3 get a subtle bar so they still stand out without the hashes.
func borgMarkdownStyle() ansi.StyleConfig {
	s := styles.DarkStyleConfig // a value copy; mutating Prefix won't touch the original
	s.H2.Prefix = "▌ "
	s.H3.Prefix = "▌ "
	s.H4.Prefix = ""
	s.H5.Prefix = ""
	s.H6.Prefix = ""
	return s
}

// mdRenderer renders markdown to ANSI for the terminal, caching a glamour
// renderer and rebuilding it only when the wrap width changes (re-rendering the
// streaming buffer on every token would otherwise allocate a renderer per call).
type mdRenderer struct {
	width int
	r     *glamour.TermRenderer
}

func newMDRenderer() *mdRenderer { return &mdRenderer{} }

// render returns s rendered as markdown wrapped to width, or s unchanged if a
// renderer can't be built (so we never drop the model's output).
func (m *mdRenderer) render(s string, width int) string {
	if width <= 0 {
		width = defaultWidth
	}
	if m.r == nil || width != m.width {
		r, err := glamour.NewTermRenderer(
			glamour.WithStyles(borgMarkdownStyle()),
			glamour.WithWordWrap(width),
		)
		if err != nil {
			return s
		}
		m.r, m.width = r, width
	}
	out, err := m.r.Render(s)
	if err != nil {
		return s
	}
	return out
}
