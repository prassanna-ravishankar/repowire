import Image from "next/image";

export default function DashboardShot() {
  return (
    <section className="section dashboard-shot" id="dashboard">
      <header className="section-head">
        <p className="section-num">§ 8</p>
        <h2>The whole mesh, in one tab.</h2>
        <p className="section-sub">
          Tail every ask, ack and notify as it happens. Open a peer to read its turns, steer it, or
          step in, from your desk or your phone.
        </p>
      </header>
      <figure className="shot">
        <div className="shot-chrome">
          <span className="shot-dots" aria-hidden>
            <span />
            <span />
            <span />
          </span>
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
      </figure>
    </section>
  );
}
