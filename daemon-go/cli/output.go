package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

func printPeers(result map[string]any) {
	width := 0
	if term.IsTerminal(int(os.Stdout.Fd())) {
		width, _, _ = term.GetSize(int(os.Stdout.Fd()))
	}
	renderPeers(os.Stdout, result, width)
}

// renderPeers keeps redirected output stable TSV while making interactive
// output fit the current terminal instead of emitting one enormous row.
func renderPeers(w io.Writer, result map[string]any, width int) {
	peers := anySlice(result["peers"])
	if width <= 0 {
		fmt.Fprintln(w, "peer_id\tname\tproject\tcircle\trole\tstatus\tpath\tbackend\tturn_state\tmodel")
		for _, raw := range peers {
			p, _ := raw.(map[string]any)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", peerField(p, "peer_id"), peerName(p), peerProject(p), peerField(p, "circle"), peerField(p, "role"), peerField(p, "status"), peerField(p, "path"), peerField(p, "backend"), peerField(p, "turn_state"), peerField(p, "model"))
		}
		return
	}
	if len(peers) == 0 {
		fmt.Fprintln(w, "No peers.")
		return
	}

	if width < 96 {
		for i, raw := range peers {
			p, _ := raw.(map[string]any)
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, truncate(peerName(p)+"  "+peerField(p, "status"), width))
			fmt.Fprintln(w, truncate("  "+strings.Join(nonempty(peerField(p, "backend"), peerField(p, "circle"), peerProject(p)), " · "), width))
			fmt.Fprintln(w, truncate("  "+peerField(p, "peer_id")+"  "+peerField(p, "path"), width))
		}
		return
	}

	columns := []int{24, 9, 13, 16, 18}
	if width < 120 {
		columns = []int{22, 9, 12, width - 49}
		writePeerRow(w, columns, "NAME", "STATUS", "BACKEND", "CIRCLE")
		for _, raw := range peers {
			p, _ := raw.(map[string]any)
			writePeerRow(w, columns, peerName(p), peerField(p, "status"), peerField(p, "backend"), peerField(p, "circle"))
		}
		return
	}
	columns[4] += width - 88
	writePeerRow(w, columns, "NAME", "STATUS", "BACKEND", "CIRCLE", "PROJECT / PATH")
	for _, raw := range peers {
		p, _ := raw.(map[string]any)
		location := strings.TrimSpace(peerProject(p) + "  " + peerField(p, "path"))
		writePeerRow(w, columns, peerName(p), peerField(p, "status"), peerField(p, "backend"), peerField(p, "circle"), location)
	}
}

func writePeerRow(w io.Writer, widths []int, values ...string) {
	for i, value := range values {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		value = truncate(value, widths[i])
		if i < len(values)-1 {
			fmt.Fprintf(w, "%-*s", widths[i], value)
		} else {
			fmt.Fprint(w, value)
		}
	}
	fmt.Fprintln(w)
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func nonempty(values ...string) []string {
	out := values[:0]
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func peerName(p map[string]any) string {
	return first(peerField(p, "display_name"), peerField(p, "name"))
}
func peerField(p map[string]any, key string) string { return stringValue(p, key) }
func peerProject(p map[string]any) string {
	if meta, ok := p["metadata"].(map[string]any); ok {
		return stringValue(meta, "project")
	}
	return ""
}
