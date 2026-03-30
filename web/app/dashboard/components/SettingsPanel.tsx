"use client";

import React from "react";

interface SettingsPanelProps {
  apiBase: string;
  isConnected: boolean;
}

export function SettingsPanel({ apiBase, isConnected }: SettingsPanelProps) {
  const host = apiBase.replace(/^https?:\/\//, "");

  return (
    <div className="px-6 max-w-2xl mx-auto space-y-8 py-4">
      {/* Page Title */}
      <div className="space-y-1">
        <p className="font-body text-xs uppercase tracking-[0.2em] text-cyan-400/60 font-medium">
          System Core
        </p>
        <h2 className="font-headline text-3xl font-bold tracking-tight text-on-surface">
          Configuration
        </h2>
      </div>

      {/* Daemon Status Card */}
      <section className="relative group">
        <div className="absolute -top-[2px] left-0 w-full h-[2px] bg-primary" />
        <div className="bg-surface-container-low p-6 space-y-4">
          <div className="flex justify-between items-start">
            <div className="space-y-1">
              <label className="font-body text-[10px] uppercase tracking-widest text-outline">
                Service Identity
              </label>
              <h3 className="font-headline text-lg font-bold">Daemon Status</h3>
            </div>
            <div
              className={`flex items-center gap-2 px-3 py-1 rounded ${
                isConnected ? "bg-secondary/10" : "bg-error/10"
              }`}
            >
              <span
                className={`w-2 h-2 rounded-full ${
                  isConnected ? "bg-secondary shadow-[0_0_8px_#d7ffc5]" : "bg-error"
                }`}
              />
              <span
                className={`font-body text-[10px] uppercase font-bold tracking-wider ${
                  isConnected ? "text-secondary" : "text-error"
                }`}
              >
                {isConnected ? "Running" : "Disconnected"}
              </span>
            </div>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="bg-surface-container-lowest p-3 border-l border-outline-variant/20">
              <p className="font-body text-[10px] text-outline uppercase mb-1">Host Address</p>
              <p className="font-mono text-sm text-primary-fixed">{host}</p>
            </div>
            <div className="bg-surface-container-lowest p-3 border-l border-outline-variant/20">
              <p className="font-body text-[10px] text-outline uppercase mb-1">Status</p>
              <p className="font-mono text-sm text-primary-fixed">
                {isConnected ? "Active" : "Unreachable"}
              </p>
            </div>
          </div>

          <button className="w-full bg-primary-container text-on-primary-container font-body text-xs font-bold uppercase py-3 tracking-widest hover:brightness-110 active:scale-[0.98] transition-all">
            Restart Daemon
          </button>
        </div>
      </section>

      {/* Relay Section */}
      <section className="space-y-4">
        <div className="flex items-center gap-3">
          <div className="h-[1px] flex-1 bg-outline-variant/20" />
          <h4 className="font-body text-[10px] uppercase tracking-[0.3em] text-outline font-semibold">
            Relay Protocol (repowire.io)
          </h4>
          <div className="h-[1px] flex-1 bg-outline-variant/20" />
        </div>

        <div className="bg-surface-container-low p-6 space-y-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="font-headline font-semibold text-on-surface">Relay Enabled</p>
              <p className="font-body text-xs text-outline">
                Tunnel local nodes to global cloud relay
              </p>
            </div>
            <div className="relative inline-flex h-6 w-11 items-center rounded-full bg-surface-container-highest cursor-pointer">
              <span className="inline-block h-4 w-4 transform translate-x-1 rounded-full bg-outline transition-transform" />
            </div>
          </div>

          <div className="space-y-2">
            <label className="font-body text-[10px] uppercase tracking-widest text-outline">
              API Key
            </label>
            <div className="relative">
              <input
                className="w-full bg-surface-container-lowest border border-outline-variant/20 font-mono text-sm px-4 py-3 focus:border-primary focus:ring-1 focus:ring-primary outline-none text-primary-fixed-dim"
                type="password"
                placeholder="rw_..."
                readOnly
              />
            </div>
          </div>
        </div>
      </section>

      {/* External Integrations */}
      <section className="space-y-4">
        <h4 className="font-body text-[10px] uppercase tracking-[0.3em] text-outline font-semibold px-2">
          External Integrations
        </h4>
        <div className="grid grid-cols-2 gap-4">
          {/* Telegram */}
          <div className="bg-surface-container-low p-5 space-y-4 border-t-2 border-secondary">
            <div className="flex items-center justify-between">
              <span className="material-symbols-outlined text-on-surface">send</span>
              <span className="font-body text-[9px] uppercase bg-secondary/10 text-secondary px-2 py-0.5 rounded">
                Configured
              </span>
            </div>
            <div>
              <p className="font-headline font-bold text-sm">Telegram Bot</p>
              <p className="font-body text-[10px] text-outline">Push notifications active</p>
            </div>
          </div>

          {/* Slack */}
          <div className="bg-surface-container-low p-5 space-y-4 border-t-2 border-outline-variant">
            <div className="flex items-center justify-between">
              <span className="material-symbols-outlined text-on-surface-variant">forum</span>
              <span className="font-body text-[9px] uppercase bg-surface-container-highest text-outline px-2 py-0.5 rounded">
                Inactive
              </span>
            </div>
            <div>
              <p className="font-headline font-bold text-sm">Slack Bot</p>
              <p className="font-body text-[10px] text-outline">Not Configured</p>
            </div>
          </div>
        </div>
      </section>

      {/* Auth Token */}
      <section className="bg-surface-container-high p-6 space-y-4">
        <div className="flex items-center gap-2">
          <span className="material-symbols-outlined text-tertiary-fixed-dim">lock</span>
          <h4 className="font-headline font-bold">Daemon Authentication</h4>
        </div>
        <div className="space-y-2">
          <label className="font-body text-[10px] uppercase tracking-widest text-outline">
            Auth Token
          </label>
          <input
            className="w-full bg-surface-container-lowest border border-outline-variant/30 font-mono text-sm px-4 py-3 focus:border-tertiary-fixed-dim focus:ring-1 focus:ring-tertiary-fixed-dim outline-none text-on-surface"
            placeholder="Set global access token..."
            type="text"
          />
          <p className="text-[10px] text-outline-variant italic">
            Required for remote CLI and dashboard access.
          </p>
        </div>
      </section>
    </div>
  );
}
