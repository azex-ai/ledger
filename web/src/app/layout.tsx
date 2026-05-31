import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";
import "@azex/ledger-react/styles.css";
import { LedgerProvider, Toaster } from "@azex/ledger-react";
import { AppSidebar } from "@/components/app-sidebar";

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

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      className={`${geistSans.variable} ${geistMono.variable} h-full antialiased dark`}
    >
      <body className="h-full">
        <LedgerProvider
          config={{
            baseUrl: process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080",
            apiKey: process.env.NEXT_PUBLIC_API_KEY,
          }}
        >
          <div className="flex h-full">
            <AppSidebar />
            <main className="flex-1 overflow-y-auto p-6 pt-16 lg:pt-6">
              {children}
            </main>
          </div>
          <Toaster
            theme="dark"
            position="bottom-right"
            toastOptions={{
              style: {
                background: "var(--card)",
                border: "1px solid var(--border)",
                color: "var(--card-foreground)",
              },
            }}
          />
        </LedgerProvider>
      </body>
    </html>
  );
}
