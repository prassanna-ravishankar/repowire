import Image from "next/image";

export default function DashboardShot() {
  return (
    <section className="dashboard-shot">
      <div className="section-head">
        <span className="eyebrow">The dashboard</span>
        <h2>See the whole mesh in your browser.</h2>
        <p className="section-sub">
          Tail every ask, ack, and notify in real time. Open a peer to read its turns, steer it, or
          step in — from your desk or your phone.
        </p>
      </div>
      <div className="shot-frame">
        <div className="shot-chrome">
          <div className="shot-dots">
            <span />
            <span />
            <span />
          </div>
          <span className="shot-url">relay.repowire.io/dashboard</span>
        </div>
        <Image
          src="/screenshots/dashboard.png"
          width={1440}
          height={900}
          alt="Repowire dashboard showing the live peer mesh, roster, and a peer conversation"
          sizes="(max-width: 980px) 100vw, 1200px"
          className="shot-img"
        />
      </div>
    </section>
  );
}
