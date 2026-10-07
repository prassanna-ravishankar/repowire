/**
 * Mesh: four terminals float over a slab wired as a mesh. The pointer picks a
 * terminal; it lifts, and its message runs out through the mesh, node by node
 * along the shortest routes to the other three, staggered by hop count. Each
 * receiver rises a little and prints a new line when the message reaches it. Left alone,
 * the terminals take turns sending; reduced motion holds the first turn. The
 * slider is the step per hop, in ms.
 *
 * The pattern: discrete items, with the stagger measured in hops rather than
 * in index, and a hit test on the terminals' rest centres.
 */
import HL from "./kernel";

const {
  Cam, circ, facing, fillet, fit, poly, prism, proj, rings, seg,
  reducedMotion, tdone, tset, tval, tween, disposer, flatDot, mk, place, pointer, put, register, solid,
} = HL;

const NODES = [
  [10, 20], [55, 6], [102, 22], [146, 10], [30, 62], [76, 50], [124, 62],
  [8, 106], [52, 98], [98, 96], [146, 104], [26, 144], [74, 142], [122, 140],
];
const LINK = 52, TERM = [1, 7, 13, 10], Z0 = [30, 20, 24, 34];
const W = 56, H = 38, TK = 1.8, LIFT = 18, BOB = 7, TURN = 2.6, PB = 6, LINES = [[0.62, 0.4], [0.82, 0.3], [0.5, 0.55]];

const links = [];
NODES.forEach((a, i) => NODES.forEach((b, j) => { if (i < j && Math.hypot(a[0] - b[0], a[1] - b[1]) < LINK) links.push([i, j]); }));

/** Hops from node s to every node, and the parent of each on a shortest route. */
function bfs(s) {
  const hop = NODES.map(() => Infinity), par = NODES.map(() => -1), q = [s];
  hop[s] = 0;
  while (q.length) {
    const a = q.shift();
    for (const [i, j] of links) {
      const b = i === a ? j : j === a ? i : -1;
      if (b >= 0 && hop[b] === Infinity) { hop[b] = hop[a] + 1; par[b] = a; q.push(b); }
    }
  }
  return { hop, par };
}

/** The nodes on the routes from terminal t's node to each of `to`. */
function routes(t, to) {
  const { hop, par } = bfs(TERM[t]), on = new Set();
  for (const u of to) for (let n = TERM[u]; n >= 0; n = par[n]) on.add(n);
  return { hop, on };
}

function mount({ stage, svg, read }, value) {
  const bag = disposer();
  let step = value;

  const C = Cam(45, 0.5, 1.42);
  const fitPts = [[-14, -14, -PB], [164, 164, -PB], [164, -14, -PB], [-14, 164, -PB]];
  TERM.forEach((n, t) => fitPts.push([NODES[n][0] - W / 2, NODES[n][1], Z0[t] + H + LIFT], [NODES[n][0] + W / 2, NODES[n][1], Z0[t] + H + LIFT]));
  fit(C, fitPts, 200, 166);
  const P = proj(C), front = facing(C);

  // The mesh layer: a slab, its links, a socket under each terminal, a node at every junction.
  const g = mk("g", {}, svg);
  const [sr, si] = rings(-14, -14, 164, 164, 14, 2.2);
  put(solid(g), prism(P, front, sr, si, -PB, 0));
  const G = (n) => P(NODES[n][0], NODES[n][1], 0);
  const linkEl = links.map(([i, j]) => mk("path", { d: seg(G(i), G(j)), class: "nf lo dash" }, g));
  const sock = circ(5, 12);
  TERM.forEach((n) => mk("path", { d: poly(sock.map((q) => P(NODES[n][0] + q.u, NODES[n][1] + q.v, 0))), class: "nf" }, g));
  const nodes = NODES.map((_p, n) => {
    const el = flatDot(g, C, 1.6, "dot off");
    place(el, G(n));
    return { el, lit: tween(0), cls: "dot off" };
  });

  // The terminal layer, back to front: a dashed drop to its socket, the plate's thickness, its face, its lines.
  const shape = fillet([[0, 0], [W, 0], [W, H], [0, H]], [3, 3, 3, 3]);
  const order = TERM.map((_n, t) => t).sort((a, b) => NODES[TERM[a]][0] + NODES[TERM[a]][1] - NODES[TERM[b]][0] - NODES[TERM[b]][1]);
  const terms = TERM.map(() => null);
  for (const t of order) {
    const grp = mk("g", {}, g);
    const drop = mk("path", { class: "nf dash" }, grp);
    const back = mk("path", { class: "lo" }, grp), face = mk("path", { class: "sil" }, grp);
    const bar = mk("path", { class: "nf lo" }, grp), text = mk("path", { class: "nf lo" }, grp), reply = mk("path", { class: "nf" }, grp);
    const lights = [0, 1, 2].map(() => mk("circle", { r: 0.9, class: "dot off" }, grp));
    const cursor = mk("circle", { r: 1.3, class: "dot off" }, grp);
    terms[t] = { drop, back, face, bar, text, reply, lights, cursor, z: tween(0), r: tween(0), drawn: "" };
  }

  function drawTerm(t, now) {
    const T = terms[t], lift = tval(T.z, now), r = tval(T.r, now), key = lift.toFixed(2) + "|" + r.toFixed(3);
    if (key === T.drawn) return;
    T.drawn = key;
    const [nx, ny] = NODES[TERM[t]], x0 = nx - W / 2, z0 = Z0[t] + lift;
    const w = (u, v) => P(x0 + u, ny, z0 + v), wb = (u, v) => P(x0 + u, ny - TK, z0 + v);
    T.drop.setAttribute("d", seg(P(nx, ny - TK / 2, z0), P(nx, ny, 0)));
    T.back.setAttribute("d", poly(shape.map((p) => wb(p[0], p[1]))));
    T.face.setAttribute("d", poly(shape.map((p) => w(p[0], p[1]))));
    T.bar.setAttribute("d", seg(w(2.5, H - 6), w(W - 2.5, H - 6)));
    T.lights.forEach((el, k) => place(el, w(4.5 + k * 3.2, H - 3)));
    const [a, b] = LINES[t % 3];
    T.text.setAttribute("d", [seg(w(5, H - 11), w(5 + (W - 10) * a, H - 11)), seg(w(5, H - 16), w(5 + (W - 10) * b, H - 16)), seg(w(5, H - 21), w(5 + (W - 10) * 0.7, H - 21))].join(""));
    const end = 5 + (W - 14) * r;
    T.reply.setAttribute("d", r > 0.02 ? seg(w(5, 6), w(end, 6)) : "");
    place(T.cursor, w(end + 3, 6));
  }

  function drawNode(nd, now) {
    const cls = tval(nd.lit, now) > 0.5 ? "dot m" : "dot off";
    if (cls !== nd.cls) { nd.cls = cls; nd.el.setAttribute("class", cls); }
  }
  function drawLinks() {
    links.forEach(([i, j], k) => {
      const on = nodes[i].cls === "dot m" && nodes[j].cls === "dot m" && lit.has(i) && lit.has(j);
      linkEl[k].setAttribute("class", on ? "nf" : "nf lo dash");
    });
  }

  // Nobody pointing: the terminals take turns, one send every TURN seconds. Ambient, so it runs only while visible.
  let clock = 0, turn = 0, act = null, lit = new Set();
  const B = register(stage, (dt, now) => {
    let moving = false;
    if (act < 0 && !reducedMotion()) {
      moving = true;
      clock += dt;
      if (clock > TURN) { clock = 0; turn = (turn + 1) % TERM.length; send(turn, false); }
    }
    terms.forEach((T, t) => { drawTerm(t, now); if (!tdone(T.z, now) || !tdone(T.r, now)) moving = true; });
    nodes.forEach((nd) => { drawNode(nd, now); if (!tdone(nd.lit, now)) moving = true; });
    drawLinks();
    return moving;
  });
  bag.add(B.unregister);

  // Hit test: the terminal whose resting face centre is nearest the pointer, within reach. Lifts never move it.
  const centre = TERM.map((n, t) => P(NODES[n][0], NODES[n][1], Z0[t] + H / 2));
  const hit = ([x, y]) => {
    let best = -1, bd = 40;
    centre.forEach((c, t) => { const d = Math.hypot(c[0] - x, c[1] - y); if (d < bd) { bd = d; best = t; } });
    return best;
  };

  /** Terminal src sends to the other three; `picked` when the pointer chose it, which gives it the bright edge. */
  function send(src, picked) {
    const now = performance.now(), { hop, on } = routes(src, TERM.map((_n, t) => t).filter((t) => t !== src));
    lit = on;
    nodes.forEach((nd, n) => tset(nd.lit, on.has(n) ? 1 : 0, now, (on.has(n) ? hop[n] : 0) * step));
    terms.forEach((T, t) => {
      const got = t !== src, arrive = (hop[TERM[t]] + 1) * step;
      tset(T.z, got ? BOB : LIFT, now, got ? arrive : 0);
      tset(T.r, got ? 1 : 0, now, got ? arrive : 0);
      T.face.classList.toggle("hi", picked && t === src);
      T.cursor.setAttribute("class", t === src ? "dot" : "dot off");
    });
    B.wake();
  }
  /** The pointer picks terminal a; -1 hands the mesh back to the turns, from where they left off. */
  function setActive(a) {
    if (a === act) return;
    act = a;
    if (a >= 0) turn = a;
    clock = 0;
    send(turn, a >= 0);
    read.textContent = a < 0 ? "rest" : `peer ${a + 1}`;
  }

  setActive(-1);
  bag.add(pointer(stage, { move: (p) => setActive(hit(p)), leave: () => setActive(-1) }));
  bag.add(() => svg.replaceChildren());

  return {
    set: (v) => { step = v; },
    destroy: bag.dispose,
  };
}

/** The figure, in the shape the hairline-create skill's bench mounts. */
export const mesh = {
  name: "mesh",
  means: "Terminals over a mesh: the one under the pointer speaks, and its message runs hop by hop to the others.",
  rules: [1, 2, 4, 6],
  range: [25, 55, 100],
  mount,
};
