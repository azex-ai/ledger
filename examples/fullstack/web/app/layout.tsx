import type { Metadata } from "next";
import "@azex/ledger-react/styles.css";

export const metadata: Metadata = {
  title: "Ledger Fullstack Example",
  description: "Next.js scaffold + @azex/ledger-react admin dashboard",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
