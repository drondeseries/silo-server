import { useQueries } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "@/api/client";
import type { FileVersion, VersionLivenessResponse } from "@/api/types";
import { isVirtualFileVersion } from "@/pages/ItemDetail/components/versionFormatUtils";

/** Maximum file_ids per versions/check request. */
export const VERSION_LIVENESS_BATCH_SIZE = 40;

/** How long a liveness result is trusted before it is re-checked. */
export const VERSION_LIVENESS_STALE_MS = 5 * 60 * 1000;

export async function fetchVersionLiveness(
  fileIds: number[],
  options?: RequestInit,
): Promise<VersionLivenessResponse> {
  return api<VersionLivenessResponse>("/catalog/versions/check", {
    ...options,
    method: "POST",
    headers: { "Content-Type": "application/json", ...(options?.headers ?? {}) },
    body: JSON.stringify({ file_ids: fileIds }),
  });
}

/**
 * Batches the virtual file_ids of `versions` into chunks of at most
 * VERSION_LIVENESS_BATCH_SIZE, each sorted ascending so the query key is
 * stable regardless of the order the caller passes versions in.
 */
export function chunkVirtualFileIds(versions: FileVersion[]): number[][] {
  const fileIds = versions
    .filter((version) => isVirtualFileVersion(version))
    .map((version) => version.file_id)
    .sort((a, b) => a - b);

  const chunks: number[][] = [];
  for (let i = 0; i < fileIds.length; i += VERSION_LIVENESS_BATCH_SIZE) {
    chunks.push(fileIds.slice(i, i + VERSION_LIVENESS_BATCH_SIZE));
  }
  return chunks;
}

/**
 * Merges the batched liveness results over the item's own metadata. A version
 * is unavailable when the item metadata says so (`available === false`) or a
 * check result reports it unavailable. Virtual versions with no result yet are
 * unknown (absent from the map). Non-virtual versions are always available.
 */
export function mergeVersionLiveness(
  versions: FileVersion[],
  results: VersionLivenessResponse | undefined,
): Map<number, boolean> {
  const availability = new Map<number, boolean>();
  for (const version of versions) {
    if (version.available === false) {
      availability.set(version.file_id, false);
    }
  }
  for (const result of results?.results ?? []) {
    // Item metadata `available: false` is authoritative; a check result only
    // fills in or refines versions the metadata did not already mark.
    if (availability.get(result.file_id) === false) continue;
    availability.set(result.file_id, result.available);
  }
  return availability;
}

/**
 * Returns a Map<file_id, available> for the given versions. The check only
 * covers virtual versions (local files are always available) and only fires
 * while `enabled` is true and there is at least one virtual version without a
 * fresh result. Results are cached for VERSION_LIVENESS_STALE_MS, keyed on the
 * sorted file_ids, so opening the version list repeatedly does not re-fetch.
 */
export function useVersionLiveness(
  versions: FileVersion[],
  enabled: boolean,
): Map<number, boolean> {
  const chunks = useMemo(() => chunkVirtualFileIds(versions), [versions]);

  const queryResults = useQueries({
    queries: chunks.map((fileIds) => ({
      queryKey: ["catalog", "versions", "check", fileIds],
      queryFn: () => fetchVersionLiveness(fileIds),
      enabled: enabled && fileIds.length > 0,
      staleTime: VERSION_LIVENESS_STALE_MS,
    })),
  });

  // Depend on the individual query data values (referentially stable while a
  // chunk's result is unchanged) rather than the queryResults array, so the
  // returned Map keeps its identity across unrelated re-renders.
  return useMemo(
    () =>
      mergeVersionLiveness(versions, {
        results: queryResults.flatMap((query) => query.data?.results ?? []),
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [versions, ...queryResults.map((query) => query.data)],
  );
}
