package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The wire view's text form, shared by the MCP tools and the CLI. Every
// field that can come from an agent is made safe for a TSV cell.

const wireShowBudget = 8000 // characters per wire_show response

// WireCell makes a value safe for one TSV cell: control characters
// (tabs and newlines included) become spaces.
func WireCell(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func wireParty(p WireParty) string {
	if p.PeerID == "" {
		return "-"
	}
	name := "@" + WireCell(string(p.Name))
	if !p.Known {
		name += "(gone)"
	}
	return name
}

func wireAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Text is the page as TSV, one thread per line, ending with a cursor line
// when there is more.
func (page WirePage) Text(now time.Time) string {
	var b strings.Builder
	b.WriteString("thread\tkind\tfrom\tholder\tvia\tstate\tage\tholder_turns\tstuck\tprotected")
	for _, t := range page.Threads {
		via := make([]string, 0, len(t.Via))
		for _, p := range t.Via {
			via = append(via, wireParty(p))
		}
		turns := "-"
		if t.HolderTurns >= 0 {
			turns = strconv.Itoa(t.HolderTurns)
		}
		fmt.Fprintf(&b, "\n%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%t",
			WireCell(t.ID), t.Kind, wireParty(t.From), wireParty(t.Holder), orDash(strings.Join(via, ",")),
			WireCell(t.State), wireAge(now.Sub(t.CreatedAt)), turns, orDash(strings.Join(t.Stuck, ",")), t.Protected)
	}
	if len(page.Threads) == 0 {
		b.WriteString("\n(no threads)")
	}
	if page.Cursor != "" {
		b.WriteString("\ncursor\t" + page.Cursor)
	}
	return b.String()
}

// Text is the thread's envelope and its stage timeline after stage seq
// after, within wireShowBudget characters; a cut timeline ends with a cursor
// line naming the last seq shown.
func (t WireThread) Text(after int) string {
	var b strings.Builder
	via := make([]string, 0, len(t.Via))
	for _, p := range t.Via {
		via = append(via, wireParty(p))
	}
	fmt.Fprintf(&b, "thread\t%s\nkind\t%s\nstate\t%s\nfrom\t%s\t%s\nholder\t%s\t%s\nvia\t%s\ncreated\t%s\nassigned\t%s\nstuck\t%s\nprotected\t%t\n\nseq\tat\tstage\tstatus\tpeer\tclose_reason",
		WireCell(t.ID), t.Kind, WireCell(t.State), wireParty(t.From), WireCell(t.From.Circle), wireParty(t.Holder), WireCell(t.Holder.Circle),
		orDash(strings.Join(via, ",")), t.CreatedAt.Format(time.RFC3339), t.AssignedAt.Format(time.RFC3339),
		orDash(strings.Join(t.Stuck, ",")), t.Protected)
	for _, st := range t.Stages {
		if st.Seq <= after {
			continue
		}
		line := fmt.Sprintf("\n%d\t%s\t%s\t%s\t%s\t%s", st.Seq, WireCell(st.At), WireCell(st.Stage), WireCell(st.Status), wireParty(st.Peer), orDash(WireCell(st.CloseReason)))
		if b.Len()+len(line) > wireShowBudget {
			fmt.Fprintf(&b, "\ncursor\t%d", st.Seq-1)
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
