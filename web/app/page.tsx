import { Geist, Geist_Mono, Instrument_Serif } from "next/font/google";
import "./marketing.css";
import TopBar from "@/components/marketing/TopBar";
import Hero from "@/components/marketing/Hero";
import SessionReplay from "@/components/marketing/SessionReplay";
import Verbs from "@/components/marketing/Verbs";
import Chapters from "@/components/marketing/Chapters";
import UseCases from "@/components/marketing/UseCases";
import DashboardShot from "@/components/marketing/DashboardShot";
import Install from "@/components/marketing/Install";
import Footer from "@/components/marketing/Footer";

// The marketing site's type, scoped to it: the dashboard keeps its own.
const display = Instrument_Serif({ variable: "--mk-display", subsets: ["latin"], weight: "400", style: ["normal", "italic"], display: "swap" });
const sans = Geist({ variable: "--mk-sans", subsets: ["latin"], display: "swap" });
const mono = Geist_Mono({ variable: "--mk-mono", subsets: ["latin"], display: "swap" });

export default function Home() {
  return (
    <div className={`rw-marketing ${display.variable} ${sans.variable} ${mono.variable}`}>
      <TopBar />
      <main>
        <Hero />
        <SessionReplay />
        <Verbs />
        <Chapters />
        <UseCases />
        <DashboardShot />
        <Install />
      </main>
      <Footer />
    </div>
  );
}
