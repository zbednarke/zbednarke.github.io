(() => {
  "use strict";

  const MAX_CLIPS = 24;
  const MAX_DURATION_MS = 10 * 60 * 1000;
  const MIN_SOURCE_CLIP_MS = 500;
  const DEFAULT_SOURCE_CLIP_MS = 10 * 1000;

  function defaultTitle(dateKey) {
    const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(dateKey || ""));
    const date = match ? new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3])) : new Date();
    return `Jazz Practice — ${date.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })}`;
  }

  function createProject(dateKey) {
    return { version: 1, title: defaultTitle(dateKey), clips: [] };
  }

  function normalizeProject(value, dateKey) {
    const fallback = createProject(dateKey);
    if (!value || value.version !== 1 || !Array.isArray(value.clips)) return fallback;
    const clips = value.clips.filter((clip) => clip && typeof clip.id === "string" && typeof clip.recordingId === "string" && Number.isFinite(clip.startMs) && Number.isFinite(clip.endMs) && clip.endMs > clip.startMs).slice(0, MAX_CLIPS);
    return { version: 1, title: String(value.title || fallback.title).slice(0, 120), clips };
  }

  function addClip(project, clip) {
    if (project.clips.length >= MAX_CLIPS) throw new Error("The output timeline is limited to 24 clips.");
    const next = { ...project, clips: [...project.clips, { ...clip }] };
    if (totalDurationMS(next) > MAX_DURATION_MS) throw new Error("The output timeline is limited to ten minutes.");
    return next;
  }

  function moveClip(project, fromIndex, toIndex) {
    if (fromIndex === toIndex || fromIndex < 0 || toIndex < 0 || fromIndex >= project.clips.length || toIndex >= project.clips.length) return project;
    const clips = [...project.clips];
    const [clip] = clips.splice(fromIndex, 1);
    clips.splice(toIndex, 0, clip);
    return { ...project, clips };
  }

  function removeClip(project, id) {
    return { ...project, clips: project.clips.filter((clip) => clip.id !== id) };
  }

  function candidateIsLiked(candidate) {
    return candidate?.reviewStatus === "kept";
  }

  function nextLikeStatus(candidate) {
    return candidateIsLiked(candidate) ? "suggested" : "kept";
  }

  function totalDurationMS(project) {
    return project.clips.reduce((sum, clip) => sum + Math.max(0, Number(clip.endMs) - Number(clip.startMs)), 0);
  }

  function placeDefaultSourceClip(clickMS, durationMS, clips, targetLengthMS = DEFAULT_SOURCE_CLIP_MS) {
    const duration = Math.max(0, Math.round(Number(durationMS) || 0));
    if (duration < MIN_SOURCE_CLIP_MS) return null;
    const click = Math.max(0, Math.min(duration, Math.round(Number(clickMS) || 0)));
    const intervals = (Array.isArray(clips) ? clips : [])
      .filter((clip) => clip && clip.reviewStatus !== "rejected" && Number.isFinite(Number(clip.startMs)) && Number.isFinite(Number(clip.endMs)))
      .map((clip) => ({ startMs: Math.max(0, Math.min(duration, Math.round(Number(clip.startMs)))), endMs: Math.max(0, Math.min(duration, Math.round(Number(clip.endMs)))) }))
      .filter((clip) => clip.endMs > clip.startMs)
      .sort((left, right) => left.startMs - right.startMs || left.endMs - right.endMs);
    if (intervals.some((clip) => click >= clip.startMs && click <= clip.endMs)) return null;

    const occupied = [];
    intervals.forEach((clip) => {
      const previous = occupied[occupied.length - 1];
      if (previous && clip.startMs <= previous.endMs) previous.endMs = Math.max(previous.endMs, clip.endMs);
      else occupied.push({ ...clip });
    });
    const gaps = [];
    let cursor = 0;
    occupied.forEach((clip) => {
      if (clip.startMs > cursor) gaps.push({ startMs: cursor, endMs: clip.startMs });
      cursor = Math.max(cursor, clip.endMs);
    });
    if (cursor < duration) gaps.push({ startMs: cursor, endMs: duration });

    const length = Math.min(duration, Math.max(MIN_SOURCE_CLIP_MS, Math.round(Number(targetLengthMS) || DEFAULT_SOURCE_CLIP_MS)));
    return gaps
      .filter((gap) => gap.endMs - gap.startMs >= length)
      .map((gap) => {
        const startMs = Math.max(gap.startMs, Math.min(click - length / 2, gap.endMs - length));
        const roundedStart = Math.round(startMs);
        return { startMs: roundedStart, endMs: roundedStart + length, distance: Math.abs(roundedStart + length / 2 - click) };
      })
      .sort((left, right) => left.distance - right.distance || left.startMs - right.startMs)
      .map(({ startMs, endMs }) => ({ startMs, endMs }))[0] || null;
  }

  function renderPayload(project) {
    if (!project.clips.length) throw new Error("Add at least one clip before rendering.");
    if (!String(project.title || "").trim()) throw new Error("Give the output a project title.");
    return {
      title: String(project.title).trim(),
      clips: project.clips.map((clip) => ({ recordingId: clip.recordingId, startMs: clip.startMs, endMs: clip.endMs })),
    };
  }

  const api = { DEFAULT_SOURCE_CLIP_MS, MIN_SOURCE_CLIP_MS, MAX_CLIPS, MAX_DURATION_MS, addClip, candidateIsLiked, createProject, defaultTitle, moveClip, nextLikeStatus, normalizeProject, placeDefaultSourceClip, removeClip, renderPayload, totalDurationMS };
  globalThis.JazzClipStudioModel = api;
  if (typeof module !== "undefined" && module.exports) module.exports = api;
})();
