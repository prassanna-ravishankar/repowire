const CASES = [
  { title: "Cross-agent review", body: "Ask the peer nearest the code to review a component, a migration or a branch before you merge it." },
  { title: "Repo-to-repo handoff", body: "Ask the API repo what changed, then let the frontend peer adapt to the answer it acks back." },
  { title: "Live context lookup", body: "Ask another session for the exact file, endpoint, schema or command output from its own checkout." },
  { title: "Checkpoint with closure", body: "Status that has to land. An open ask keeps resurfacing until the peer closes it." },
  { title: "A second opinion", body: "Have a different backend critique the approach while the first agent keeps the thread moving." },
  { title: "Human escalation", body: "Route the same ask through Telegram, Slack or the dashboard when a person has to decide." },
];

export default function UseCases() {
  return (
    <section className="section" id="use-cases">
      <header className="section-head">
        <h2>What people wire up first.</h2>
      </header>
      <ol className="cases">
        {CASES.map((c) => (
          <li key={c.title}>
            <h3>{c.title}</h3>
            <p>{c.body}</p>
          </li>
        ))}
      </ol>
    </section>
  );
}
