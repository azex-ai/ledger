import { render, screen, within } from "@testing-library/react";
import { describe, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { Sidebar } from "../../src/components/sidebar";
import { LEDGER_NAV_ITEMS } from "../../src/components/nav";

const linkItems = LEDGER_NAV_ITEMS.filter(
  (i): i is Extract<typeof i, { href: string }> => i.type !== "separator",
);

describe("Sidebar", () => {
  test("renders every nav item label", () => {
    render(<Sidebar pathname="/" />);
    for (const item of linkItems) {
      // Each label appears in both mobile + desktop variants.
      expect(screen.getAllByText(item.label).length).toBeGreaterThan(0);
    }
  });

  test("uses the provided linkComponent for nav links", () => {
    const linkComponent = vi.fn(
      ({ href, className, children }: { href: string; className?: string; children: ReactNode }) => (
        <a data-testid="custom-link" href={href} className={className}>
          {children}
        </a>
      ),
    );

    render(<Sidebar pathname="/" linkComponent={linkComponent} />);

    expect(linkComponent).toHaveBeenCalled();
    // The mobile drawer is closed initially, so only the desktop nav renders
    // its full list of links.
    const links = screen.getAllByTestId("custom-link");
    expect(links.length).toBe(linkItems.length);
  });

  test("highlights the active item by pathname", () => {
    render(<Sidebar pathname="/journals" />);

    const journalsLinks = screen.getAllByRole("link", { name: "Journals" });
    expect(journalsLinks.length).toBeGreaterThan(0);
    for (const link of journalsLinks) {
      expect(link.className).toContain("border-primary");
    }

    // A non-active item should not carry the active highlight.
    const balancesLinks = screen.getAllByRole("link", { name: "Balances" });
    for (const link of balancesLinks) {
      expect(link.className).not.toContain("border-primary");
    }
  });

  test("root '/' is active only on exact match, not as a prefix", () => {
    render(<Sidebar pathname="/journals" />);
    const dashboardLinks = screen.getAllByRole("link", { name: "Dashboard" });
    for (const link of dashboardLinks) {
      expect(link.className).not.toContain("border-primary");
    }
  });
});
