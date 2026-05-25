export default function ConceptsPage() {
  return (
    <main>
      <h1>Repowire mesh command concepts</h1>
      <p>
        Core commands include status, peers, pending-asks, ask, notify,
        schedule, timeline, result, and doctor. Machine responses carry a
        schema_version field where applicable.
      </p>
      <p>
        Agents must use Repowire ask/ack/notify tools rather than SendMessage
        for mesh peers. The tracked-work lifecycle is separate from ask
        receipt, and ACP/channel broker health remains a distinct readiness
        concern.
      </p>
      <p>
        Claude Code marketplace plugin packaging is only a convenience layer;
        it does not replace repowire setup.
      </p>
    </main>
  );
}
