// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it } from "vitest";
import { AudioTrackMenu } from "./AudioTrackMenu";

function renderMenu(tracks: Parameters<typeof AudioTrackMenu>[0]["tracks"]) {
  return render(
    createElement(AudioTrackMenu, {
      tracks,
      activeIndex: 0,
      onSelect: () => {},
      currentPosition: 0,
      open: true,
      onOpenChange: () => {},
      hideTrigger: true,
    }),
  );
}

describe("AudioTrackMenu", () => {
  it("joins the languages array into a multi-language descriptor", () => {
    renderMenu([
      {
        title: "English / French / Spanish",
        languages: ["en", "fr", "es"],
        codec: "eac3",
        layout: "5.1",
      },
    ]);
    expect(screen.getByText("English/French/Spanish · 5.1")).toBeTruthy();
  });

  it("falls back to the single language tag when languages is absent", () => {
    renderMenu([{ title: "French DTS", language: "fra", codec: "dts" }]);
    expect(screen.getByText("French")).toBeTruthy();
  });

  it("omits the language segment when no language resolves", () => {
    renderMenu([{ title: "Commentary", codec: "ac3", layout: "2.0" }]);
    expect(screen.getByText("2.0")).toBeTruthy();
  });
});
