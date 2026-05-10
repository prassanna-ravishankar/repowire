import Link from "next/link";

export default function Footer() {
  return (
    <footer className="bg-surface-dim">
      <div className="mx-auto flex max-w-7xl flex-col gap-4 px-4 py-10 sm:flex-row sm:items-center sm:justify-between sm:px-6 lg:px-8">
        <p className="font-mono text-xs text-outline">
          &copy; {new Date().getFullYear()} Repowire. Open source MIT License.
        </p>
        <Link
          href="https://github.com/prassanna-ravishankar/repowire"
          className="font-mono text-xs uppercase tracking-[0.14em] text-outline transition-colors hover:text-primary-fixed"
        >
          GitHub
        </Link>
      </div>
    </footer>
  );
}
