export function toMediaTime(playerTimeSeconds: number, streamOriginSeconds = 0): number {
  return Math.max(0, playerTimeSeconds + streamOriginSeconds);
}

export function toPlayerTime(mediaTimeSeconds: number, streamOriginSeconds = 0): number {
  return Math.max(0, mediaTimeSeconds - streamOriginSeconds);
}

export function mediaElementDuration(
  backendDurationSeconds: number,
  elementDurationSeconds: number,
): number | null {
  if (backendDurationSeconds > 0) return null;
  if (!Number.isFinite(elementDurationSeconds) || elementDurationSeconds <= 0) return null;
  return elementDurationSeconds;
}
