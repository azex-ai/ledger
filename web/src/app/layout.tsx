import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";
import "@azex/ledger-react/styles.css";
import { LedgerProviders } from "@/components/ledger-providers";
import { AppSidebar } from "@/components/app-sidebar";
import { getDashboardCacheScope } from "@/lib/ledger-cache-scope";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  title: "Ledger Admin",
  description: "Double-entry ledger management dashboard",
};

export default async function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  const cacheScope = await getDashboardCacheScope();
  return (
    <html
      lang="en"
      className={`${geistSans.variable} ${geistMono.variable} h-full antialiased dark`}
    >
      <body className="h-full">
        <LedgerProviders cacheScope={cacheScope}>
          <div className="flex h-full">
            <AppSidebar />
            <main className="flex-1 overflow-y-auto p-6 pt-16 lg:pt-6">
              {children}
            </main>
          </div>
        </LedgerProviders>
      </body>
    </html>
  );
}
