import Navbar from '@/components/Navbar';
import Hero from '@/components/Hero';
import Features from '@/components/Features';
import HowItWorks from '@/components/HowItWorks';
import Installation from '@/components/Installation';
import Footer from '@/components/Footer';

export default function Home() {
  return (
    <main className="min-h-screen bg-surface text-on-surface selection:bg-primary/30 mesh-bg">
      <Navbar />
      <Hero />
      <Features />
      <HowItWorks />
      <Installation />
      <Footer />
    </main>
  );
}
