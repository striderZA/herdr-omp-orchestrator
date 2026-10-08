package board

import (
	"fmt"
	"strings"
)

// pageLines draws every fact of one task, wrapped to width. It is the body of
// the dedicated detail page: unlike the collapsed summary it omits nothing, and
// the page scrolls when it is taller than the terminal.
func pageLines(card Card, width int, th Theme) []string {
	if width < 1 {
		width = 1
	}
	t := card.Task
	var out []string
	add := func(label, value string) {
		out = append(out, wrap(label+": "+orNone(singleLine(value), "none"), width)...)
	}
	section := func(name string) {
		out = append(out, "", th.paint(Ink, clip(name, width)))
	}

	out = append(out, th.paint(Ink, clip(card.ID()+th.separator()+strings.ToLower(StageLabel(card.Stage)), width)))
	add("Title", t.Title)
	add("Branch", t.Branch)
	add("Worker", card.Pane())
	if line := ticketLine(card); line != "" {
		out = append(out, wrap(line, width)...)
	}
	if observed := card.Observation(); observed != "" {
		add("Last observation", observed)
	}

	if len(card.DependencyBlocks) > 0 {
		section("Needs a human")
		for _, reason := range card.DependencyBlocks {
			add("Blocked", reason)
		}
	}
	if question := card.Attention(); question != "" {
		section("Needs a human")
		label := "Attention"
		if strings.TrimSpace(t.Blocker) != "" {
			label = "Blocked"
		}
		add(label, question)
	}

	section("Brief")
	out = append(out, wrap(orNone(strings.TrimSpace(t.Brief), "none"), width)...)

	if report := t.Report; report != nil {
		section("Report")
		add("Kind", report.Kind)
		add("Summary", report.Summary)
		for _, item := range report.Evidence {
			out = append(out, wrap("Evidence: "+singleLine(item), width)...)
		}
		for _, item := range report.Checks {
			out = append(out, wrap("Check: "+singleLine(item.Label()), width)...)
		}
		if risk := singleLine(report.Risk); risk != "" {
			add("Risk", risk)
		}
	}

	if len(t.Checks) > 0 || len(t.Dependencies) > 0 || len(t.ReservedPaths) > 0 || strings.TrimSpace(t.Disposition) != "" || strings.TrimSpace(t.Outcome) != "" {
		section("Plan")
		for _, item := range t.Checks {
			out = append(out, wrap("Required check: "+singleLine(item), width)...)
		}
		for _, item := range t.Dependencies {
			out = append(out, wrap("Depends on: "+singleLine(item), width)...)
		}
		for _, item := range t.ReservedPaths {
			out = append(out, wrap("Reserved path: "+singleLine(item), width)...)
		}
		if o := singleLine(t.Outcome); o != "" {
			add("Outcome", o)
		} else if strings.TrimSpace(t.Disposition) != "" {
			add("Outcome", "legacy (counted as accepted)")
		}
		if d := singleLine(t.Disposition); d != "" {
			add("Disposition", d)
		}
	}
	return out
}

// pageWindow is the slice of lines that fits in rows, starting at scroll, and
// the scroll actually used. rows < 0 means the terminal height is unknown, so
// everything is shown.
func pageWindow(lines []string, scroll, rows int) ([]string, int) {
	if rows < 0 || len(lines) <= rows {
		return lines, 0
	}
	if max := len(lines) - rows; scroll > max {
		scroll = max
	}
	if scroll < 0 {
		scroll = 0
	}
	return lines[scroll : scroll+rows], scroll
}

// pageChrome decides what shares the terminal with the page body. The help line
// always stays; the rule and a notice need a terminal at least minRuledHeight
// rows tall, so a tiny terminal keeps the help and as much body as fits. rows is
// the body budget, or -1 when the height is unknown. Both the render and the
// scroll clamp use it, so they can never disagree.
func pageChrome(height int, hasNotice bool) (rows int, rule, notice bool) {
	if height < 1 {
		return -1, true, hasNotice
	}
	footer := 1
	if height >= minRuledHeight {
		rule = true
		notice = hasNotice
		footer++
		if notice {
			footer++
		}
	}
	rows = height - footer
	if rows < 0 {
		rows = 0
	}
	return rows, rule, notice
}

// minRuledHeight is the shortest terminal that still gets the rule and notices.
const minRuledHeight = 4

// RenderPage draws the dedicated detail page for one task, replacing the board.
// Esc returns to the board; scroll moves through a page taller than height.
//
// The primary marker leads the page body, so opening a task does not hide that
// the primary is unverified. It is part of the scrollable body (pageLinesOf
// measures it too), not extra chrome, so render and scroll clamp still agree.
func RenderPage(b *Board, d Detail, width, height int, th Theme) string {
	return renderPageWithSearch(b, d, width, height, th, "")
}

func renderPageWithSearch(b *Board, d Detail, width, height int, th Theme, query string) string {
	if width < 1 {
		width = 1
	}
	query = strings.TrimSpace(query)
	marker := primaryMarkerBlock(b, width, th)
	var all []string
	help := "Esc back, f focus, q close"
	if ticket, isTicket := b.FindTicket(d.SelectedID); isTicket {
		all = ticketPageLines(ticket, width, th)
		// A ticket has no worker pane, so there is nothing to focus.
		help = strings.Join(append(append([]string{"Esc back", "/ search"}, ticketWriteKeys(ticket, d)...), "q close"), ", ")
	} else if card, ok := b.Find(d.SelectedID); ok {
		all = pageLines(card, width, th)
	} else {
		lines := append([]string{}, marker...)
		if query != "" {
			lines = append(lines, clip("Filter: "+singleLine(query), width))
		}
		lines = append(lines, clip("The task is no longer on this board.", width))
		lines = append(lines, clip(searchPageHelp(query, "Esc back, q close"), width))
		return strings.Join(lines, "\n")
	}
	all = append(append([]string{}, marker...), all...)
	notice := noticeLine(d, th, width)
	rows, rule, showNotice := pageChrome(height, notice != "")
	status := ""
	if query != "" {
		if rows > 0 {
			status = clip("Filter: "+singleLine(query), width)
			rows--
		} else {
			help = searchPageHelp(query, help)
		}
	}
	body, scroll := pageWindow(all, d.Scroll, rows)
	lines := make([]string, 0, len(body)+4)
	lines = append(lines, body...)
	if showNotice {
		lines = append(lines, notice)
	}
	if status != "" {
		lines = append(lines, status)
	}
	if len(body) > 0 && len(body) < len(all) {
		help = fmt.Sprintf("%d-%d/%d up/down scroll, %s", scroll+1, scroll+len(body), len(all), help)
	}
	if rule {
		lines = append(lines, th.paint(Ink, strings.Repeat(th.rule(), width)))
	}
	lines = append(lines, clip(help, width))
	return strings.Join(lines, "\n")
}

func searchPageHelp(query, help string) string {
	if query == "" {
		return help
	}
	return "Filter: " + singleLine(query) + " | " + help
}
