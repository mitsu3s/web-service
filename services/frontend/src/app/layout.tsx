import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "DevBoard",
  description: "K8s Learning - Task Management App",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <body>{children}</body>
    </html>
  );
}
