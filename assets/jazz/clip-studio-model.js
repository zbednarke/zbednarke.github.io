(() => {
  "use strict";

  const MAX_CLIPS = 24;
  const MAX_DURATION_MS = 10 * 60 * 1000;

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

  function totalDurationMS(project) {
    return project.clips.reduce((sum, clip) => sum + Math.max(0, Number(clip.endMs) - Number(clip.startMs)), 0);
  }

  function renderPayload(project) {
    if (!project.clips.length) throw new Error("Add at least one clip before rendering.");
    if (!String(project.title || "").trim()) throw new Error("Give the output a project title.");
    return {
      title: String(project.title).trim(),
      clips: project.clips.map((clip) => ({ recordingId: clip.recordingId, startMs: clip.startMs, endMs: clip.endMs })),
    };
  }

  const api = { MAX_CLIPS, MAX_DURATION_MS, addClip, createProject, defaultTitle, moveClip, normalizeProject, removeClip, renderPayload, totalDurationMS };
  globalThis.JazzClipStudioModel = api;
  if (typeof module !== "undefined" && module.exports) module.exports = api;
})();
