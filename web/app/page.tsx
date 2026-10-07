import { Geist, Geist_Mono } from "next/font/google";
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
const sans = Geist({ variable: "--mk-sans", subsets: ["latin"], display: "swap" });
const mono = Geist_Mono({ variable: "--mk-mono", subsets: ["latin"], display: "swap" });

export default function Home() {
  return (
    <div className={`rw-marketing ${sans.variable} ${mono.variable}`}>
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
