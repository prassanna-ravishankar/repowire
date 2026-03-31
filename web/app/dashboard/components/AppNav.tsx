"use client";

import Image from "next/image";
import { cn } from "../lib/utils";

export type NavTab = "dash" | "logs" | "config";

interface AppNavProps {
  activeTab: NavTab;
  onTabChange: (tab: NavTab) => void;
  onSpawn?: () => void;
}

const tabs: { id: NavTab; icon: string; label: string }[] = [
  { id: "dash", icon: "grid_view", label: "Dash" },
  { id: "logs", icon: "lan", label: "Logs" },
  { id: "config", icon: "settings", label: "Config" },
];

export function AppNav({ activeTab, onTabChange, onSpawn }: AppNavProps) {
  return (
    <>
      {/* Desktop Side Rail (md+) */}
      <aside className="hidden md:flex fixed top-0 left-0 h-full w-64 bg-surface-container-low border-r border-outline-variant/15 flex-col z-50">
        {/* Logo */}
        <div className="p-6">
          <div className="flex items-center gap-3">
            <Image src="/logo-cyan.svg" alt="Repowire" width={28} height={28} />
            <h1 className="text-xl font-bold tracking-widest text-cyan-400 font-headline uppercase">
              REPOWIRE
            </h1>
          </div>
        </div>

        {/* Nav items */}
        <nav className="flex-1 px-3 py-4 space-y-1">
          {tabs.map((tab) => {
            const isActive = activeTab === tab.id;
            return (
              <button
                key={tab.id}
                onClick={() => onTabChange(tab.id)}
                className={cn(
                  "w-full flex items-center gap-3 px-4 py-3 rounded-lg transition-all duration-200 text-xs uppercase tracking-widest font-headline",
                  isActive
                    ? "text-cyan-400 bg-surface-container-highest border-r-2 border-primary"
                    : "text-slate-400 hover:text-cyan-400 hover:bg-surface-container-highest"
                )}
              >
                <span
                  className="material-symbols-outlined text-[20px]"
                  style={isActive ? { fontVariationSettings: "'FILL' 1" } : undefined}
                >
                  {tab.icon}
                </span>
                {tab.label}
              </button>
            );
          })}
        </nav>

        {/* Spawn button */}
        {onSpawn && (
          <div className="p-4">
            <button
              onClick={onSpawn}
              className="w-full flex items-center justify-center gap-2 py-3 bg-gradient-to-br from-primary to-primary-container text-on-primary font-headline text-xs font-bold uppercase tracking-widest rounded hover:brightness-110 active:scale-[0.98] transition-all shadow-lg shadow-cyan-400/10"
            >
              <span className="material-symbols-outlined text-[18px]">add</span>
              Deploy New Node
            </button>
          </div>
        )}
      </aside>

      {/* Mobile Bottom Tabs (< md) */}
      <nav className="md:hidden fixed bottom-0 left-0 w-full z-50 flex justify-around items-center px-4 pb-6 pt-2 bg-surface/80 backdrop-blur-xl border-t border-cyan-900/20 shadow-[0_-4px_24px_rgba(0,229,255,0.05)]">
        {tabs.map((tab) => {
          const isActive = activeTab === tab.id;
          return (
            <button
              key={tab.id}
              onClick={() => onTabChange(tab.id)}
              className={cn(
                "flex flex-col items-center justify-center p-3 active:scale-90 duration-200",
                isActive
                  ? "bg-cyan-400/10 text-cyan-400 rounded-lg"
                  : "text-slate-500 hover:text-cyan-300"
              )}
            >
              <span
                className="material-symbols-outlined"
                style={isActive ? { fontVariationSettings: "'FILL' 1" } : undefined}
              >
                {tab.icon}
              </span>
              <span className="font-body text-[10px] uppercase tracking-widest mt-1">
                {tab.label}
              </span>
            </button>
          );
        })}
      </nav>
    </>
  );
}
