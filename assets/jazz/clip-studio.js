(() => {
  "use strict";

  const API_BASE = "./api/v1";
  const STORAGE_PREFIX = "jazz.clip-studio.output.v1";
  const U = globalThis.JazzArchiveUtils;
  const Model = globalThis.JazzClipStudioModel;
  if (!U || !Model) return;
  const $ = (selector, root = document) => root.querySelector(selector);
  const state = {
    date: U.dateKey(new Date()), initialized: false, recordings: [], candidates: [], analysis: null,
    current: null, currentRecording: null, currentMode: "idle", media: null,
    loadRequest: 0, playbackRequest: 0, savePromise: Promise.resolve(), saveTimers: new Map(),
    project: Model.createProject(U.dateKey(new Date())), draggedOutputIndex: -1,
  };

  async function api(path, options = {}) {
    const response = await fetch(`${API_BASE}${path}`, {
      ...options,
      headers: { ...(options.body ? { "Content-Type": "application/json" } : {}), ...(options.headers || {}) },
    });
    const body = response.status === 204 ? null : await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body?.error || `Request failed (${response.status})`);
    return body;
  }

  function escapeHTML(value) {
    const span = document.createElement("span");
    span.textContent = String(value ?? "");
    return span.innerHTML;
  }

  function recordingFor(value) {
    const recordingID = value?.recordingId || value?.RecordingID;
    return state.recordings.find((recording) => recording.id === recordingID);
  }

  function titleFor(recording) {
    return recording?.practiceBlockTitle || recording?.practiceSessionTitle || "Open practice";
  }

  function formatClock(milliseconds) {
    return U.formatPlaybackTime(Math.max(0, Number(milliseconds || 0)) / 1000);
  }

  function setStatus(message, tone = "") {
    const element = $("#studio-status");
    element.textContent = message;
    element.dataset.tone = tone;
  }

  function setRenderStatus(message, tone = "") {
    const element = $("#studio-render-status");
    element.textContent = message;
    element.dataset.tone = tone;
  }

  function localID() {
    if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
    return `clip-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  }

  function projectStorageKey(date = state.date) {
    return `${STORAGE_PREFIX}:${date}`;
  }

  function loadProject() {
    try {
      const raw = localStorage.getItem(projectStorageKey());
      state.project = Model.normalizeProject(raw ? JSON.parse(raw) : null, state.date);
    } catch {
      state.project = Model.createProject(state.date);
    }
    $("#studio-output-title").value = state.project.title;
    renderOutputTimeline();
  }

  function persistProject() {
    localStorage.setItem(projectStorageKey(), JSON.stringify(state.project));
  }

  function initialize() {
    if (state.initialized) return;
    state.initialized = true;
    loadProject();
    $("#studio-date").value = state.date;
    $("#studio-date").max = U.dateKey(new Date());
    $("#studio-date").addEventListener("change", (event) => loadDay(event.target.value));
    $("#studio-previous-day").addEventListener("click", () => shiftDay(-1));
    $("#studio-next-day").addEventListener("click", () => shiftDay(1));
    $("#studio-scan").addEventListener("click", () => scanDay(false));
    $("#studio-start").addEventListener("input", () => updateBoundaryFromInputs("start"));
    $("#studio-end").addEventListener("input", () => updateBoundaryFromInputs("end"));
    $("#studio-candidate-notes").addEventListener("input", () => scheduleNoteSave());
    $("#studio-candidate-notes").addEventListener("blur", () => saveNoteNow());
    $("#studio-add-output").onclick = addCurrentToOutput;
    $("#studio-reject").onclick = rejectCurrent;
    $("#studio-output-title").addEventListener("input", (event) => {
      state.project = { ...state.project, title: event.target.value.slice(0, 120) };
      persistProject();
    });
    $("#studio-render").onclick = renderOutputMovie;
    loadDay(state.date);
  }

  function shiftDay(delta) {
    const date = U.parseDateKey(state.date);
    if (!date) return;
    date.setDate(date.getDate() + delta);
    const next = U.dateKey(date);
    if (next <= U.dateKey(new Date())) loadDay(next);
  }

  async function loadDay(date) {
    if (!U.parseDateKey(date)) return;
    const request = ++state.loadRequest;
    if (state.date !== date) persistProject();
    state.date = date;
    loadProject();
    $("#studio-date").value = date;
    setStatus("Loading lossless masters");
    setScanState("checking", "Checking analysis", "Loading this day’s analysis state…");
    try {
      const result = await api(`/studio/days/${date}`);
      if (request !== state.loadRequest) return;
      state.recordings = result.recordings || [];
      state.candidates = result.candidates || [];
      state.analysis = result.analysis || { needsScan: true };
      state.current = null;
      state.currentRecording = null;
      closePreview();
      render();
      if (state.recordings.length && state.analysis.needsScan) {
        await scanDay(true);
      } else if (state.recordings.length) {
        setScanState("complete", "Scan complete", scanDetail());
        setStatus("Analysis active", "success");
      } else {
        setScanState("idle", "Nothing to scan", "No completed recordings on this day.");
        setStatus("No completed recordings");
      }
    } catch (error) {
      if (request !== state.loadRequest) return;
      state.recordings = [];
      state.candidates = [];
      render();
      setScanState("error", "Scan unavailable", error.message);
      setStatus(`Studio unavailable · ${error.message}`, "error");
    }
  }

  function scanDetail() {
    const analyzedAt = state.analysis?.analyzedAt ? new Date(state.analysis.analyzedAt) : null;
    const when = analyzedAt && !Number.isNaN(analyzedAt.valueOf()) ? analyzedAt.toLocaleString() : "just now";
    return `Waveform activity analysis completed ${when}. Rescanning regenerates unreviewed suggestions.`;
  }

  function setScanState(tone, label, detail) {
    const menu = $("#studio-scan-menu");
    menu.dataset.tone = tone;
    $("#studio-scan-state").textContent = label;
    $("#studio-scan-detail").textContent = detail;
  }

  async function scanDay(automatic) {
    if (!state.recordings.length) return;
    const scanDate = state.date;
    const button = $("#studio-scan");
    button.disabled = true;
    setScanState("working", automatic ? "Analyzing automatically" : "Rescanning", "Finding sustained musical activity in the lossless masters…");
    setStatus("Analysis running");
    try {
      const result = await api(`/studio/days/${scanDate}/scan`, { method: "POST", body: "{}" });
      if (state.date !== scanDate) return;
      state.candidates = result.candidates || [];
      state.analysis = { needsScan: false, analyzedAt: new Date().toISOString(), version: "waveform-v1", recordingCount: result.scannedRecordings };
      state.current = null;
      state.currentRecording = null;
      closePreview();
      render();
      setScanState("complete", "Scan complete", scanDetail());
      setStatus("Analysis active", "success");
      $("#studio-scan-menu").open = false;
    } catch (error) {
      if (state.date !== scanDate) return;
      setScanState("error", "Scan failed", error.message);
      setStatus(`Scan failed · ${error.message}`, "error");
    } finally {
      button.disabled = false;
    }
  }

  function render() {
    const duration = state.recordings.reduce((sum, recording) => sum + Number(recording.durationMs || 0), 0);
    $("#studio-total-time").textContent = formatClock(duration);
    $("#studio-take-count").textContent = String(state.recordings.length);
    $("#studio-candidate-count").textContent = String(state.candidates.filter((item) => item.reviewStatus !== "rejected").length);
    renderRecordings();
    renderCandidates();
  }

  function renderRecordings() {
    const host = $("#studio-recordings");
    if (!state.recordings.length) {
      host.innerHTML = '<p class="clip-studio-empty">No completed recordings on this practice day.</p>';
      return;
    }
    host.innerHTML = state.recordings.map((recording) => recordingMarkup(recording)).join("");
    host.querySelectorAll("[data-studio-candidate]").forEach((button) => button.addEventListener("click", (event) => {
      event.stopPropagation();
      selectCandidate(button.dataset.studioCandidate, true);
    }));
    host.querySelectorAll("[data-boundary-candidate]").forEach((handle) => handle.addEventListener("pointerdown", (event) => beginBoundaryDrag(event, handle)));
    host.querySelectorAll(".clip-studio-waveform").forEach((waveform) => waveform.addEventListener("click", (event) => {
      if (event.target.closest("[data-studio-candidate],[data-boundary-candidate]")) return;
      const recording = state.recordings.find((item) => item.id === waveform.dataset.recordingId);
      if (!recording) return;
      const bounds = waveform.getBoundingClientRect();
      const percentage = Math.max(0, Math.min(1, (event.clientX - bounds.left) / bounds.width));
      browseRecording(recording, Math.round(Number(recording.durationMs || 0) * percentage));
    }));
  }

  function recordingMarkup(recording) {
    const candidates = state.candidates.filter((candidate) => candidate.recordingId === recording.id && candidate.reviewStatus !== "rejected");
    const duration = Math.max(1, Number(recording.durationMs || 0));
    const peaks = Array.isArray(recording.waveformPeaks) ? recording.waveformPeaks : [];
    const barCount = Math.min(120, peaks.length);
    const sampled = Array.from({ length: barCount }, (_, index) => peaks[Math.floor((index / barCount) * peaks.length)] || 0);
    const waveform = sampled.map((peak) => `<i style="height:${Math.max(3, Math.round(Number(peak) * 100))}%"></i>`).join("");
    const regions = candidates.map((candidate) => regionMarkup(candidate, duration)).join("");
    return `<article class="clip-studio-recording"><header><div><strong>${escapeHTML(titleFor(recording))}</strong><span>${escapeHTML(U.takeLabel(recording))} · ${recording.mediaKind === "video" ? "video + lossless WAV" : "lossless WAV"}</span></div><time>${formatClock(duration)}</time></header><div class="clip-studio-waveform" data-recording-id="${recording.id}" aria-label="${escapeHTML(titleFor(recording))} ${escapeHTML(U.takeLabel(recording))} waveform"><div class="studio-waveform-bars">${waveform}</div><div class="studio-waveform-regions">${regions}</div><div class="studio-playhead" data-playhead-recording="${recording.id}" hidden><i></i></div></div></article>`;
  }

  function regionMarkup(candidate, duration) {
    const left = Math.max(0, Math.min(100, candidate.startMs / duration * 100));
    const right = Math.max(left, Math.min(100, candidate.endMs / duration * 100));
    return `<div class="studio-clip-region${state.current?.id === candidate.id ? " active" : ""}" data-region-id="${candidate.id}" style="left:${left}%;width:${Math.max(.35, right - left)}%"><button class="studio-region-hit" type="button" data-studio-candidate="${candidate.id}" aria-label="Edit candidate ${formatClock(candidate.startMs)} to ${formatClock(candidate.endMs)}"></button><button class="studio-clip-boundary left" type="button" data-boundary-candidate="${candidate.id}" data-boundary-edge="start" aria-label="Drag candidate in point"></button><button class="studio-clip-boundary right" type="button" data-boundary-candidate="${candidate.id}" data-boundary-edge="end" aria-label="Drag candidate out point"></button></div>`;
  }

  function renderCandidates() {
    const host = $("#studio-candidate-list");
    if (!state.candidates.length) {
      host.innerHTML = `<p class="clip-studio-empty">${state.recordings.length ? "Analysis found no candidate regions yet." : "Recordings from the selected day will appear here."}</p>`;
      return;
    }
    host.innerHTML = state.candidates.map((candidate, index) => {
      const recording = recordingFor(candidate);
      const reasons = Array.isArray(candidate.reasons) ? candidate.reasons : [];
      const label = candidate.reviewStatus === "rejected" ? "Rejected" : `Suggestion ${index + 1}`;
      return `<article class="clip-candidate ${candidate.reviewStatus}${state.current?.id === candidate.id ? " active" : ""}"><button class="clip-candidate-open" type="button" data-open-candidate="${candidate.id}"><span>${label}</span><strong>${escapeHTML(titleFor(recording))} · ${escapeHTML(U.takeLabel(recording))}</strong><time>${formatClock(candidate.startMs)} — ${formatClock(candidate.endMs)}</time><em>${Math.round(Number(candidate.score || 0) * 100)}% activity confidence</em></button><div class="clip-candidate-reasons">${reasons.map((reason) => `<span>${escapeHTML(reason)}</span>`).join("")}</div></article>`;
    }).join("");
    host.querySelectorAll("[data-open-candidate]").forEach((button) => button.addEventListener("click", () => selectCandidate(button.dataset.openCandidate, true)));
  }

  async function selectCandidate(id, autoplay) {
    const candidate = state.candidates.find((item) => item.id === id);
    const recording = recordingFor(candidate);
    if (!candidate || !recording) return;
    state.current = candidate;
    state.currentRecording = recording;
    state.currentMode = "candidate";
    renderCandidates();
    updateRegionSelection();
    $("#studio-playback-mode").textContent = "Editing clip";
    $("#studio-preview-title").textContent = `${titleFor(recording)} · ${U.takeLabel(recording)}`;
    $("#studio-start").value = (candidate.startMs / 1000).toFixed(1);
    $("#studio-end").value = (candidate.endMs / 1000).toFixed(1);
    $("#studio-candidate-notes").value = candidate.notes || "";
    $("#studio-editor").hidden = false;
    $("#studio-raw-notice").hidden = true;
    await loadPlayer(recording, candidate.startMs, autoplay);
  }

  async function browseRecording(recording, startMS) {
    state.current = null;
    state.currentRecording = recording;
    state.currentMode = "raw";
    renderCandidates();
    updateRegionSelection();
    $("#studio-playback-mode").textContent = "Browsing raw recording";
    $("#studio-preview-title").textContent = `${titleFor(recording)} · ${U.takeLabel(recording)}`;
    $("#studio-editor").hidden = true;
    const notice = $("#studio-raw-notice");
    notice.textContent = "Browsing raw recording · playback continues until you pause it.";
    notice.hidden = false;
    await loadPlayer(recording, startMS, true);
  }

  async function previewOutputClip(clip) {
    const recording = recordingFor(clip);
    if (!recording) {
      setRenderStatus("This source take is not part of the selected day.", "error");
      return;
    }
    state.current = null;
    state.currentRecording = recording;
    state.currentMode = "output";
    renderCandidates();
    updateRegionSelection();
    $("#studio-playback-mode").textContent = "Output clip preview";
    $("#studio-preview-title").textContent = `${clip.title} · ${formatClock(clip.startMs)}–${formatClock(clip.endMs)}`;
    $("#studio-editor").hidden = true;
    const notice = $("#studio-raw-notice");
    notice.textContent = "Output clip preview · this is the copied cut currently in the render timeline.";
    notice.hidden = false;
    await loadPlayer(recording, clip.startMs, true);
  }

  async function loadPlayer(recording, startMS, autoplay) {
    const request = ++state.playbackRequest;
    $("#studio-media").innerHTML = '<p class="clip-studio-empty">Loading private playback…</p>';
    try {
      const asset = recording.mediaKind === "video" ? "video" : "audio";
      const result = await api(`/recordings/${recording.id}/playback-url?asset=${asset}`, { method: "POST", body: "{}" });
      if (request !== state.playbackRequest || state.currentRecording?.id !== recording.id) return;
      state.media?.pause();
      const media = document.createElement(asset === "video" ? "video" : "audio");
      media.controls = true;
      media.preload = "metadata";
      media.playsInline = true;
      media.src = result.url;
      media.addEventListener("loadedmetadata", () => {
        media.currentTime = Math.max(0, Math.min(Number(media.duration || Infinity), Number(startMS || 0) / 1000));
        if (autoplay) media.play().catch(() => {});
        updatePlayhead();
      }, { once: true });
      ["timeupdate", "seeking", "seeked", "play", "pause"].forEach((event) => media.addEventListener(event, updatePlayhead));
      $("#studio-media").replaceChildren(media);
      state.media = media;
    } catch (error) {
      $("#studio-media").innerHTML = `<p class="clip-studio-empty">Playback unavailable · ${escapeHTML(error.message)}</p>`;
    }
  }

  function updatePlayhead() {
    document.querySelectorAll("[data-playhead-recording]").forEach((playhead) => {
      const active = state.currentRecording && playhead.dataset.playheadRecording === state.currentRecording.id && state.media;
      playhead.hidden = !active;
      if (!active) return;
      const duration = Math.max(1, Number(state.currentRecording.durationMs || 0) / 1000);
      playhead.style.left = `${Math.max(0, Math.min(100, Number(state.media.currentTime || 0) / duration * 100))}%`;
    });
  }

  function beginBoundaryDrag(event, handle) {
    const candidate = state.candidates.find((item) => item.id === handle.dataset.boundaryCandidate);
    const recording = recordingFor(candidate);
    const waveform = handle.closest(".clip-studio-waveform");
    const region = handle.closest(".studio-clip-region");
    if (!candidate || !recording || !waveform || !region) return;
    event.preventDefault();
    event.stopPropagation();
    selectCandidate(candidate.id, false);
    handle.setPointerCapture?.(event.pointerId);
    handle.classList.add("dragging");
    const edge = handle.dataset.boundaryEdge;
    const duration = Math.max(1, Number(recording.durationMs || 0));
    const minimumGap = Math.max(500, Math.round(duration * .003));
    const update = (pointerEvent) => {
      const bounds = waveform.getBoundingClientRect();
      const nextMS = Math.round(Math.max(0, Math.min(1, (pointerEvent.clientX - bounds.left) / bounds.width)) * duration);
      if (edge === "start") candidate.startMs = Math.min(nextMS, candidate.endMs - minimumGap);
      else candidate.endMs = Math.max(nextMS, candidate.startMs + minimumGap);
      updateRegion(candidate, region, duration);
      $("#studio-start").value = (candidate.startMs / 1000).toFixed(1);
      $("#studio-end").value = (candidate.endMs / 1000).toFixed(1);
      scheduleBoundarySave(candidate, 650, false);
    };
    const finish = () => {
      handle.classList.remove("dragging");
      handle.removeEventListener("pointermove", update);
      handle.removeEventListener("pointerup", finish);
      handle.removeEventListener("pointercancel", finish);
      scheduleBoundarySave(candidate, 250, true);
    };
    handle.addEventListener("pointermove", update);
    handle.addEventListener("pointerup", finish);
    handle.addEventListener("pointercancel", finish);
  }

  function updateRegion(candidate, region, duration) {
    const left = candidate.startMs / duration * 100;
    const right = candidate.endMs / duration * 100;
    region.style.left = `${left}%`;
    region.style.width = `${Math.max(.35, right - left)}%`;
  }

  function updateRegionSelection() {
    document.querySelectorAll("[data-region-id]").forEach((region) => region.classList.toggle("active", region.dataset.regionId === state.current?.id));
  }

  function updateBoundaryFromInputs(edge) {
    const candidate = state.current;
    const recording = state.currentRecording;
    if (!candidate || !recording) return;
    const duration = Number(recording.durationMs || 0);
    const value = Math.round(Number(edge === "start" ? $("#studio-start").value : $("#studio-end").value) * 1000);
    if (!Number.isFinite(value)) return;
    if (edge === "start") candidate.startMs = Math.max(0, Math.min(value, candidate.endMs - 500));
    else candidate.endMs = Math.min(duration, Math.max(value, candidate.startMs + 500));
    $("#studio-start").value = (candidate.startMs / 1000).toFixed(1);
    $("#studio-end").value = (candidate.endMs / 1000).toFixed(1);
    const region = document.querySelector(`[data-region-id="${candidate.id}"]`);
    if (region) updateRegion(candidate, region, Math.max(1, duration));
    scheduleBoundarySave(candidate, 650, true);
  }

  function scheduleBoundarySave(candidate, delay, refresh) {
    const key = `boundary:${candidate.id}`;
    clearTimeout(state.saveTimers.get(key));
    state.saveTimers.set(key, setTimeout(() => {
      state.saveTimers.delete(key);
      patchCandidate(candidate, { startMs: candidate.startMs, endMs: candidate.endMs }, "Clip boundaries synced", refresh);
    }, delay));
  }

  function scheduleNoteSave() {
    if (!state.current) return;
    const key = `note:${state.current.id}`;
    clearTimeout(state.saveTimers.get(key));
    state.saveTimers.set(key, setTimeout(saveNoteNow, 700));
  }

  function saveNoteNow() {
    const candidate = state.current;
    if (!candidate) return;
    const key = `note:${candidate.id}`;
    clearTimeout(state.saveTimers.get(key));
    state.saveTimers.delete(key);
    patchCandidate(candidate, { notes: $("#studio-candidate-notes").value.trim() }, "Editor note synced", false);
  }

  function patchCandidate(candidate, payload, successMessage, refresh) {
    state.savePromise = state.savePromise.catch(() => {}).then(async () => {
      try {
        const result = await api(`/studio/candidates/${candidate.id}`, { method: "PATCH", body: JSON.stringify(payload) });
        Object.assign(candidate, result);
        setStatus(successMessage, "success");
        if (refresh) {
          renderCandidates();
          renderRecordings();
          updatePlayhead();
        }
      } catch (error) {
        setStatus(`Could not save · ${error.message}`, "error");
      }
    });
    return state.savePromise;
  }

  async function rejectCurrent() {
    const candidate = state.current;
    if (!candidate) return;
    await patchCandidate(candidate, { reviewStatus: "rejected", notes: $("#studio-candidate-notes").value.trim() }, "Suggestion rejected", true);
  }

  function addCurrentToOutput() {
    const candidate = state.current;
    const recording = state.currentRecording;
    if (!candidate || !recording) return;
    if (recording.mediaKind !== "video") {
      setRenderStatus("Only takes with video can be added to a movie.", "error");
      return;
    }
    const clip = {
      id: localID(), candidateId: candidate.id, recordingId: recording.id,
      startMs: candidate.startMs, endMs: candidate.endMs, title: titleFor(recording),
      takeNumber: recording.takeNumber || 0, practiceDate: recording.practiceDate || state.date,
    };
    try {
      state.project = Model.addClip(state.project, clip);
      persistProject();
      renderOutputTimeline();
      setRenderStatus(`Added ${clip.title} · ${U.takeLabel(recording)} using the current boundaries.`, "success");
    } catch (error) {
      setRenderStatus(error.message, "error");
    }
  }

  function renderOutputTimeline() {
    const host = $("#studio-output-clips");
    $("#studio-output-duration").textContent = formatClock(Model.totalDurationMS(state.project));
    $("#studio-render").disabled = !state.project.clips.length;
    if (!state.project.clips.length) {
      host.innerHTML = "<p>Use Add to Output to begin an edit.</p>";
      return;
    }
    host.innerHTML = state.project.clips.map((clip, index) => {
      const take = clip.takeNumber ? ` · Take ${clip.takeNumber}` : "";
      return `<article class="studio-output-clip" draggable="true" data-output-index="${index}" data-output-id="${clip.id}" style="--clip-weight:${Math.max(1, clip.endMs - clip.startMs)}"><button class="studio-output-grip" type="button" data-output-grip aria-label="Drag to reorder ${escapeHTML(clip.title)}">⠿</button><button class="studio-output-open" type="button" data-output-open="${clip.id}"><strong>${escapeHTML(clip.title)}${take}</strong><span>${formatClock(clip.startMs)} — ${formatClock(clip.endMs)}</span></button><button class="studio-output-remove" type="button" data-output-remove="${clip.id}" aria-label="Remove ${escapeHTML(clip.title)} from output">×</button></article>`;
    }).join("");
    host.querySelectorAll("[data-output-open]").forEach((button) => button.addEventListener("click", () => {
      const clip = state.project.clips.find((item) => item.id === button.dataset.outputOpen);
      if (clip) previewOutputClip(clip);
    }));
    host.querySelectorAll("[data-output-remove]").forEach((button) => button.addEventListener("click", () => {
      state.project = Model.removeClip(state.project, button.dataset.outputRemove);
      persistProject();
      renderOutputTimeline();
    }));
    host.querySelectorAll("[data-output-grip]").forEach((grip) => grip.addEventListener("pointerdown", (event) => beginOutputPointerDrag(event, grip)));
    host.querySelectorAll("[data-output-index]").forEach((clip) => {
      clip.addEventListener("dragstart", () => { state.draggedOutputIndex = Number(clip.dataset.outputIndex); clip.classList.add("dragging"); });
      clip.addEventListener("dragend", () => { state.draggedOutputIndex = -1; clip.classList.remove("dragging"); });
      clip.addEventListener("dragover", (event) => { event.preventDefault(); clip.classList.add("drop-target"); });
      clip.addEventListener("dragleave", () => clip.classList.remove("drop-target"));
      clip.addEventListener("drop", (event) => {
        event.preventDefault();
        const target = Number(clip.dataset.outputIndex);
        state.project = Model.moveClip(state.project, state.draggedOutputIndex, target);
        persistProject();
        renderOutputTimeline();
      });
    });
  }

  function beginOutputPointerDrag(event, grip) {
    const source = grip.closest("[data-output-index]");
    if (!source) return;
    event.preventDefault();
    let targetIndex = Number(source.dataset.outputIndex);
    const clips = [...document.querySelectorAll(".studio-output-clip")];
    source.classList.add("dragging");
    grip.setPointerCapture?.(event.pointerId);
    const update = (pointerEvent) => {
      let nearest = targetIndex;
      let distance = Infinity;
      clips.forEach((clip) => {
        const bounds = clip.getBoundingClientRect();
        const nextDistance = Math.abs(pointerEvent.clientX - (bounds.left + bounds.width / 2));
        if (nextDistance < distance) {
          distance = nextDistance;
          nearest = Number(clip.dataset.outputIndex);
        }
      });
      targetIndex = nearest;
      clips.forEach((clip) => clip.classList.toggle("drop-target", Number(clip.dataset.outputIndex) === targetIndex));
    };
    const finish = () => {
      grip.removeEventListener("pointermove", update);
      grip.removeEventListener("pointerup", finish);
      grip.removeEventListener("pointercancel", finish);
      clips.forEach((clip) => clip.classList.remove("dragging", "drop-target"));
      state.project = Model.moveClip(state.project, Number(source.dataset.outputIndex), targetIndex);
      persistProject();
      renderOutputTimeline();
    };
    grip.addEventListener("pointermove", update);
    grip.addEventListener("pointerup", finish);
    grip.addEventListener("pointercancel", finish);
  }

  async function renderOutputMovie() {
    state.project = { ...state.project, title: $("#studio-output-title").value.slice(0, 120) };
    persistProject();
    let payload;
    try {
      payload = Model.renderPayload(state.project);
    } catch (error) {
      setRenderStatus(error.message, "error");
      return;
    }
    const button = $("#studio-render");
    button.disabled = true;
    button.textContent = "Rendering…";
    setRenderStatus("Rendering 1080p video with lossless ALAC audio. Keep this page open.");
    try {
      const result = await api("/studio/renders", { method: "POST", body: JSON.stringify(payload) });
      const link = document.createElement("a");
      link.href = result.url;
      link.download = result.filename || "jazz-practice.mp4";
      link.rel = "noreferrer";
      document.body.appendChild(link);
      link.click();
      link.remove();
      const audioQuality = result.quality?.audio || "audio quality not reported";
      if (result.quality?.audioLossless === true) {
        setRenderStatus(`Downloaded ${result.filename} · ${audioQuality}.`, "success");
      } else {
        setRenderStatus(`QUALITY NOTICE · Downloaded ${result.filename} with ${audioQuality}; this output is not confirmed lossless.`, "error");
      }
    } catch (error) {
      setRenderStatus(`Render failed · ${error.message}`, "error");
    } finally {
      button.disabled = !state.project.clips.length;
      button.textContent = "Render Output";
    }
  }

  function closePreview() {
    state.media?.pause();
    state.media = null;
    state.playbackRequest++;
    $("#studio-playback-mode").textContent = "Focused playback";
    $("#studio-preview-title").textContent = "Choose a suggestion";
    $("#studio-media").innerHTML = '<p class="clip-studio-empty">Choose a suggested region, or click anywhere in a waveform to browse a complete take.</p>';
    $("#studio-editor").hidden = true;
    $("#studio-raw-notice").hidden = true;
    updatePlayhead();
  }

  document.addEventListener("jazz:view-change", (event) => {
    if (event.detail?.view === "studio") initialize();
    else state.media?.pause();
  });
  if (location.hash === "#studio") initialize();
})();
