import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import DetailHero from "./DetailHero";

vi.mock("@/lib/thumbhash", () => ({
  decodeThumbhash: (thumbhash: string) => `data:image/png;base64,${thumbhash}`,
}));

describe("DetailHero artwork revisions", () => {
  it("keeps the above-fold primary content on one bounded reveal surface", () => {
    const { container } = render(<DetailHero title="Blade Runner" />);

    expect(container.querySelector(".detail-hero-primary-content")).not.toBeNull();
  });

  it("reserves the logo box before the image decodes", () => {
    const { container } = render(
      <DetailHero title="Blade Runner" logoUrl="/blade-runner-logo.rev-a.webp" />,
    );

    const logo = container.querySelector<HTMLImageElement>(
      'img[src="/blade-runner-logo.rev-a.webp"]',
    );
    expect(logo).toHaveClass("h-20", "w-full", "lg:h-28");
    expect(logo).not.toHaveClass("max-h-20", "lg:max-h-28");
  });

  it("treats a changed poster URL as unloaded until that revision finishes loading", () => {
    const { rerender } = render(<DetailHero title="Blade Runner" posterUrl="/poster.rev-a.webp" />);

    const first = screen.getByRole("img", { name: "Blade Runner" });
    const firstPlaceholder = screen.getByTestId("detail-hero-poster-placeholder");
    expect(first).toHaveClass("opacity-0");
    expect(first).not.toHaveClass("transition-opacity");
    expect(firstPlaceholder).toHaveClass("opacity-100", "transition-opacity");
    fireEvent.load(first);
    expect(first).toHaveClass("opacity-100");
    expect(firstPlaceholder).toHaveClass("opacity-0");

    rerender(<DetailHero title="Blade Runner" posterUrl="/poster.rev-b.webp" />);

    const replacement = screen.getByRole("img", { name: "Blade Runner" });
    const replacementPlaceholder = screen.getByTestId("detail-hero-poster-placeholder");
    expect(replacement).toHaveAttribute("src", "/poster.rev-b.webp");
    expect(replacement).toHaveClass("opacity-0");
    expect(replacementPlaceholder).toHaveClass("opacity-100");
    fireEvent.load(replacement);
    expect(replacement).toHaveClass("opacity-100");
    expect(replacementPlaceholder).toHaveClass("opacity-0");
  });

  it("keeps the backdrop placeholder behind the image throughout its fade", () => {
    const { container } = render(
      <DetailHero
        title="Blade Runner"
        backdropUrl="/backdrop.rev-a.webp"
        backdropThumbhash="placeholder"
      />,
    );

    const artwork = container.querySelector<HTMLElement>(".hero-backdrop-artwork");
    const backdrop = container.querySelector<HTMLImageElement>('img[src="/backdrop.rev-a.webp"]');
    expect(artwork).toHaveStyle({
      backgroundImage: 'url("data:image/png;base64,placeholder")',
    });
    expect(backdrop).toHaveClass("opacity-0", "transition-opacity", "duration-300");

    fireEvent.load(backdrop!);

    expect(backdrop).toHaveClass("opacity-100", "transition-opacity", "duration-300");
    expect(artwork).toHaveStyle({
      backgroundImage: 'url("data:image/png;base64,placeholder")',
    });
  });
});
