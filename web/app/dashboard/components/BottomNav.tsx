"use client";

import React from "react";
import { cn } from "../lib/utils";

export type NavTab = "dash" | "logs" | "config";

interface BottomNavProps {
  activeTab: NavTab;
  onTabChange: (tab: NavTab) => void;
}

const tabs: { id: NavTab; icon: string; label: string }[] = [
  { id: "dash", icon: "grid_view", label: "Dash" },
  { id: "logs", icon: "lan", label: "Logs" },
  { id: "config", icon: "settings", label: "Config" },
];

export function BottomNav({ activeTab, onTabChange }: BottomNavProps) {
  return (
    <nav className="fixed bottom-0 left-0 w-full z-50 flex justify-around items-center px-4 pb-6 pt-2 bg-surface/80 backdrop-blur-xl border-t border-cyan-900/20 shadow-[0_-4px_24px_rgba(0,229,255,0.05)]">
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
  );
}
