import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { SortableSectionTableRow } from "./EditableSectionRows";

vi.mock("@dnd-kit/sortable", () => ({
  useSortable: () => ({
    attributes: {},
    listeners: {},
    setNodeRef: vi.fn(),
    transform: null,
    transition: undefined,
    isDragging: false,
  }),
}));

function renderRow(onSelectionChange: (checked: boolean, extendRange: boolean) => void) {
  return render(
    <table>
      <tbody>
        <SortableSectionTableRow
          section={{
            id: "section-two",
            title: "Section Two",
            sectionType: "recently_added",
            itemLimit: 12,
            featured: false,
            enabled: true,
          }}
          canReorder={false}
          libraries={[]}
          collectionLabels={new Map()}
          selected={false}
          selectionLabel="Select Section Two home section"
          onSelectionChange={onSelectionChange}
          onEdit={vi.fn()}
          onDelete={vi.fn()}
        />
      </tbody>
    </table>,
  );
}

describe("SortableSectionTableRow selection", () => {
  it("selects only its checkbox on a normal click", () => {
    const onSelectionChange = vi.fn();
    renderRow(onSelectionChange);

    fireEvent.click(screen.getByRole("checkbox", { name: "Select Section Two home section" }));

    expect(onSelectionChange).toHaveBeenCalledWith(true, false);
  });

  it("requests a range selection on a shift-click", () => {
    const onSelectionChange = vi.fn();
    renderRow(onSelectionChange);

    fireEvent.click(screen.getByRole("checkbox", { name: "Select Section Two home section" }), {
      shiftKey: true,
    });

    expect(onSelectionChange).toHaveBeenCalledWith(true, true);
  });
});
