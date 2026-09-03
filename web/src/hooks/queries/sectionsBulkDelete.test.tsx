import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiClientError } from "@/api/client";
import { sectionKeys } from "./keys";
import { useDeleteSections } from "./sections";

const apiMock = vi.hoisted(() => vi.fn());
const invalidateAdminCollectionQueriesMock = vi.hoisted(() => vi.fn());
const toastSuccessMock = vi.hoisted(() => vi.fn());
const toastWarningMock = vi.hoisted(() => vi.fn());
const toastErrorMock = vi.hoisted(() => vi.fn());

vi.mock("@/api/client", async () => {
  const actual = await vi.importActual<typeof import("@/api/client")>("@/api/client");
  return { ...actual, api: apiMock };
});

vi.mock("./collectionSurfaceRefresh", () => ({
  invalidateAdminCollectionQueries: invalidateAdminCollectionQueriesMock,
}));

vi.mock("sonner", () => ({
  toast: {
    success: toastSuccessMock,
    warning: toastWarningMock,
    error: toastErrorMock,
  },
}));

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

function renderDeleteSectionsHook() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const invalidateQueries = vi.spyOn(queryClient, "invalidateQueries");
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  return { ...renderHook(() => useDeleteSections(), { wrapper }), invalidateQueries };
}

describe("useDeleteSections", () => {
  afterEach(() => {
    vi.clearAllMocks();
  });

  it("deduplicates section IDs and limits each request batch to four", async () => {
    const firstBatch = deferred<void>();
    apiMock
      .mockReturnValueOnce(firstBatch.promise)
      .mockReturnValueOnce(firstBatch.promise)
      .mockReturnValueOnce(firstBatch.promise)
      .mockReturnValueOnce(firstBatch.promise)
      .mockResolvedValue(undefined);
    invalidateAdminCollectionQueriesMock.mockResolvedValue(undefined);
    const { result, invalidateQueries } = renderDeleteSectionsHook();

    let mutation!: Promise<unknown>;
    act(() => {
      mutation = result.current.mutateAsync([
        "section-1",
        "section-2",
        "section-3",
        "section-4",
        "section-5",
        "section-6",
        "section-1",
      ]);
    });

    await waitFor(() => expect(apiMock).toHaveBeenCalledTimes(4));
    expect(result.current.progress).toEqual({ completed: 0, total: 6 });
    await act(async () => {
      firstBatch.resolve();
      await mutation;
    });

    expect(apiMock.mock.calls).toEqual([
      ["/admin/sections/section-1", { method: "DELETE" }],
      ["/admin/sections/section-2", { method: "DELETE" }],
      ["/admin/sections/section-3", { method: "DELETE" }],
      ["/admin/sections/section-4", { method: "DELETE" }],
      ["/admin/sections/section-5", { method: "DELETE" }],
      ["/admin/sections/section-6", { method: "DELETE" }],
    ]);
    expect(result.current.progress).toBeNull();
    expect(toastSuccessMock).toHaveBeenCalledWith("Deleted 6 sections");
    expect(invalidateQueries).toHaveBeenCalledWith({ queryKey: sectionKeys.all });
    expect(invalidateAdminCollectionQueriesMock).toHaveBeenCalledTimes(1);
  });

  it("treats missing sections as deleted and reports unexpected failures", async () => {
    apiMock
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new ApiClientError(404, "not_found", "Section not found"))
      .mockRejectedValueOnce(new ApiClientError(500, "internal_error", "Database unavailable"));
    invalidateAdminCollectionQueriesMock.mockResolvedValue(undefined);
    const { result } = renderDeleteSectionsHook();

    let response: unknown;
    await act(async () => {
      response = await result.current.mutateAsync(["section-1", "section/2", "section-3"]);
    });

    expect(response).toEqual({
      requested: 3,
      deleted: 2,
      kept: 0,
      failed: 1,
      firstError: "Database unavailable",
    });
    expect(apiMock).toHaveBeenNthCalledWith(2, "/admin/sections/section%2F2", {
      method: "DELETE",
    });
    expect(toastWarningMock).toHaveBeenCalledWith("Deleted 2 of 3 sections", {
      description: "Database unavailable",
    });
    expect(toastErrorMock).not.toHaveBeenCalled();
    expect(invalidateAdminCollectionQueriesMock).toHaveBeenCalledTimes(1);
  });
});
