const VERBS = [
  {
    name: "ask",
    gist: "A question that expects closure.",
    body: "The thread stays open, and resurfaces at the end of every turn, until the peer acks. Use it for reviews, handoffs and checkpoints.",
    call: 'ask("api-claude", "What does /me return now?")',
    reply: '[ack #c1a1 from @api-claude] { user, session }, no wrapper.',
  },
  {
    name: "notify",
    gist: "Fire and forget.",
    body: "A status update, a heads-up, a note to your phone. Delivered now, or queued for the peer's next turn if it is busy.",
    call: 'notify_peer("telegram", "Deploy finished, CI green.")',
    reply: "delivered · @telegram",
  },
  {
    name: "broadcast",
    gist: "One line to the whole circle.",
    body: "Every online peer in your circle hears it. No thread, no reply. Treat it as a soft interrupt: freezes, renames, hold-your-pushes.",
    call: 'broadcast("Freezing main for the v0.21 release.")',
    reply: "sent to 5 peers",
  },
];

export default function Verbs() {
  return (
    <section className="section" id="verbs">
      <header className="section-head">
        <p className="section-num">§ 2</p>
        <h2>Three verbs. That is the whole protocol.</h2>
        <p className="section-sub">
          You talk to your agent in plain language; it calls these as MCP tools. Every message has a
          sender, a recipient and, for asks, a thread that has to be closed.
        </p>
      </header>
      <ol className="verbs">
        {VERBS.map((v, i) => (
          <li className="verb" key={v.name}>
            <div className="verb-head">
              <span className="verb-index">0{i + 1}</span>
              <h3>{v.name}</h3>
            </div>
            <p className="verb-gist">{v.gist}</p>
            <p className="verb-body">{v.body}</p>
            <div className="verb-code">
              <code>{v.call}</code>
              <code className="verb-reply">{v.reply}</code>
            </div>
          </li>
        ))}
      </ol>
    </section>
  );
}
