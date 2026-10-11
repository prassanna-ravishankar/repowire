package routing

import "fmt"

// DeliveryNote is the line prepended to a routed ask so the recipient knows
// the sender did not name it, which arm chose it, and on what evidence. A
// fallback says so too: a misroute must never look like an addressed ask.
func DeliveryNote(res RouterResult, candidates int) string {
	if res.Fallback {
		return fmt.Sprintf("[routing: %s failed (%s); delivered to the sender's fallback target]\n", res.Arm, res.FallbackReason)
	}
	because := fmt.Sprintf("you scored %.2f, the highest of %d candidates", res.Scores[0].Score, candidates)
	if len(res.Scores) > 1 {
		because += fmt.Sprintf(" (next %.2f)", res.Scores[1].Score)
	}
	return fmt.Sprintf("[routed by %s because %s; the sender did not name you, decline if this is not yours]\n", res.Arm, because)
}
