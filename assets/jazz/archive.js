(() => {
  "use strict";

  const API_BASE = "./api/v1";
  const U = globalThis.JazzArchiveUtils;
  if (!U) return;

  const $ = (selector, root = document) => root.querySelector(selector);
  let effectsResizeObserver = null;
  const state = {
    month: new Date(new Date().getFullYear(), new Date().getMonth(), 1),
    selectedDate: U.dateKey(new Date()),
    calendar: new Map(),
    day: null,
    initialized: false,
    playlist: [],
    currentIndex: -1,
    currentRecording: null,
    currentAsset: "",
    media: null,
    expectedDuration: 0,
    playbackExpiresAt: 0,
    noteTimer: null,
    noteDirty: false,
    playbackRequest: 0,
    calendarRequest: 0,
    dayRequest: 0,
  };

  async function api(path, options = {}) {
    const response = await fetch(`${API_BASE}${path}`, {
      ...options,
      headers: {
        ...(options.body ? { "Content-Type": "application/json" } : {}),
        ...(options.headers || {}),
      },
    });
    const body = response.status === 204 ? null : await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(body?.error || `Request failed (${response.status})`);
      error.status = response.status;
      throw error;
    }
    return body;
  }

  function escapeHTML(value) {
    const span = document.createElement("span");
    span.textContent = String(value ?? "");
    return span.innerHTML;
  }

  function monthLabel(date) {
    return date.toLocaleDateString(undefined, { month: "long", year: "numeric" });
  }

  function fullDateLabel(value) {
    const date = U.parseDateKey(value);
    return date ? date.toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" }) : value;
  }

  function shortRecordedAt(value) {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? "" : date.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
  }

  function formatBytes(value) {
    const bytes = Number(value || 0);
    if (!bytes) return "—";
    if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
    if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
    return `${Math.round(bytes / 1024)} KB`;
  }

  function drawPlayerWaveform(currentSeconds = 0) {
    const canvas = $("#archive-waveform-canvas");
    if (!canvas) return;
    const bounds = canvas.getBoundingClientRect();
    const ratio = Math.max(1, window.devicePixelRatio || 1);
    const width = Math.max(1, Math.round(bounds.width * ratio));
    const height = Math.max(1, Math.round(bounds.height * ratio));
    if (canvas.width !== width || canvas.height !== height) {
      canvas.width = width;
      canvas.height = height;
    }
    const context = canvas.getContext("2d");
    context.clearRect(0, 0, width, height);
    const peaks = Array.isArray(state.currentRecording?.waveformPeaks) ? state.currentRecording.waveformPeaks : [];
    const center = height / 2;
    const progress = state.expectedDuration ? Math.max(0, Math.min(1, currentSeconds / state.expectedDuration)) : 0;
    context.strokeStyle = "rgba(181,147,91,0.22)";
    context.beginPath();
    context.moveTo(0, center);
    context.lineTo(width, center);
    context.stroke();
    if (!peaks.length) return;
    const bars = Math.max(1, Math.min(peaks.length, Math.floor(width / (4 * ratio))));
    const barWidth = Math.max(1, 1.6 * ratio);
    for (let index = 0; index < bars; index += 1) {
      const start = Math.floor((index / bars) * peaks.length);
      const end = Math.max(start + 1, Math.floor(((index + 1) / bars) * peaks.length));
      let peak = 0;
      for (let source = start; source < end; source += 1) peak = Math.max(peak, Number(peaks[source]) || 0);
      const amplitude = Math.max(1.5 * ratio, peak * height * 0.43);
      const x = ((index + 0.5) / bars) * width;
      context.fillStyle = index / bars <= progress ? "#d2b77f" : "rgba(181,147,91,0.38)";
      context.fillRect(x - (barWidth / 2), center - amplitude, barWidth, amplitude * 2);
    }
  }

  function visibleView() {
    if (location.hash === "#archive") return "archive";
    if (location.hash === "#guide-tones") return "guide-tones";
    if (location.hash === "#studio") return "studio";
    if (location.hash === "#effects") return "effects";
    return "today";
  }

  function resizeEffectsFrame() {
    const frame = $("#effects-frame");
    const frameBody = frame?.contentDocument?.body;
    if (!frame || !frameBody) return;
    frame.style.height = `${Math.max(720, frameBody.scrollHeight + 2)}px`;
  }

  function watchEffectsFrameSize() {
    const frame = $("#effects-frame");
    const frameDocument = frame?.contentDocument;
    if (!frameDocument) return;
    effectsResizeObserver?.disconnect();
    effectsResizeObserver = new ResizeObserver(resizeEffectsFrame);
    if (frameDocument.body) effectsResizeObserver.observe(frameDocument.body);
    resizeEffectsFrame();
  }

  function route() {
    const view = visibleView();
    document.body.classList.toggle("virtuoso-today", view === "today");
    document.body.classList.toggle("virtuoso-archive", view === "archive");
    document.body.classList.toggle("virtuoso-guide-tones", view === "guide-tones");
    document.body.classList.toggle("virtuoso-studio", view === "studio");
    document.body.classList.toggle("virtuoso-effects", view === "effects");
    $("#today").hidden = view !== "today";
    $("#archive").hidden = view !== "archive";
    $("#guide-tones").hidden = view !== "guide-tones";
    $("#studio").hidden = view !== "studio";
    $("#effects").hidden = view !== "effects";
    const labels = { archive: "Archive", "guide-tones": "Guide tones", studio: "Clip studio", effects: "Live effects", today: "Today" };
    $("#mobile-view-label").textContent = labels[view];
    document.querySelectorAll("[data-jazz-view]").forEach((link) => {
      const active = link.dataset.jazzView === view;
      link.classList.toggle("active", active);
      if (active) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    });
    if (view === "archive" && !state.initialized) initializeArchive();
    if (view === "effects") requestAnimationFrame(resizeEffectsFrame);
    if (view !== "effects") $("#effects-frame")?.contentWindow?.postMessage({ type: "jazz:effects-stop" }, location.origin);
    document.dispatchEvent(new CustomEvent("jazz:view-change", { detail: { view } }));
  }

  async function initializeArchive() {
    state.initialized = true;
    wireArchiveControls();
    await loadMonth();
    await selectDay(state.selectedDate);
  }

  async function loadMonth() {
    const requestID = ++state.calendarRequest;
    const grid = U.monthGrid(state.month);
    const from = grid[0].key;
    const to = grid[grid.length - 1].key;
    $("#archive-calendar").setAttribute("aria-busy", "true");
    setCalendarStatus("Loading the archive…");
    try {
      let result;
      try {
        result = await api(`/archive/calendar?from=${from}&to=${to}`);
      } catch (error) {
        if (error.status !== 404) throw error;
        result = await fallbackCalendar(from, to);
      }
      if (requestID !== state.calendarRequest) return;
      state.calendar = new Map((result.days || []).map((day) => [day.date, day]));
      renderCalendar();
      setCalendarStatus("");
    } catch (error) {
      if (requestID !== state.calendarRequest) return;
      state.calendar = new Map();
      renderCalendar();
      setCalendarStatus(`Archive unavailable: ${error.message}`, "error");
    } finally {
      $("#archive-calendar").setAttribute("aria-busy", "false");
    }
  }

  async function fallbackCalendar(from, to) {
    const [sessionResult, recordingResult] = await Promise.all([api("/practice-sessions"), api("/recordings")]);
    const days = new Map();
    const ensure = (date) => {
      if (!days.has(date)) days.set(date, { date, totalDurationMs: 0, recordingCount: 0, sectionCount: 0, sessionCount: 0, hasNotes: false });
      return days.get(date);
    };
    (sessionResult.sessions || []).forEach((session) => {
      const date = U.dateKey(new Date(session.startedAt));
      if (date < from || date > to) return;
      const day = ensure(date);
      day.totalDurationMs += Number(session.totalDurationMs || 0);
      day.sessionCount += 1;
      day.hasNotes ||= Boolean(session.summary);
    });
    (recordingResult.recordings || []).forEach((recording) => {
      const date = recording.practiceDate || U.dateKey(new Date(recording.recordedAt));
      if (date < from || date > to) return;
      const day = ensure(date);
      day.recordingCount += 1;
      day.hasNotes ||= Boolean(recording.notes);
    });
    return { days: [...days.values()] };
  }

  function setCalendarStatus(message, tone = "") {
    const element = $("#archive-calendar-status");
    element.textContent = message;
    element.dataset.tone = tone;
  }

  function renderCalendar() {
    const calendar = $("#archive-calendar");
    const grid = U.monthGrid(state.month);
    const today = U.dateKey(new Date());
    calendar.replaceChildren();
    ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"].forEach((label) => {
      const header = document.createElement("div");
      header.className = "archive-weekday";
      header.setAttribute("role", "columnheader");
      header.textContent = label;
      calendar.appendChild(header);
    });
    const maxDuration = Math.max(1, ...grid.map((cell) => Number(state.calendar.get(cell.key)?.totalDurationMs || 0)));
    grid.forEach((cell) => {
      const data = state.calendar.get(cell.key) || {};
      const duration = Number(data.totalDurationMs || 0);
      const takes = Number(data.recordingCount || 0);
      const density = Math.max(duration ? 8 : 0, Math.round((duration / maxDuration) * 100));
      const button = document.createElement("button");
      button.type = "button";
      button.className = `archive-day-button${cell.inMonth ? "" : " outside"}${duration || takes ? " has-practice" : ""}${cell.key === state.selectedDate ? " selected" : ""}${cell.key === today ? " today" : ""}`;
      button.dataset.date = cell.key;
      button.setAttribute("role", "gridcell");
      button.setAttribute("aria-label", `${fullDateLabel(cell.key)}${duration ? `, ${U.formatPracticeDuration(duration)}` : ", no recorded practice"}${takes ? `, ${takes} takes` : ""}`);
      button.innerHTML = `<span class="archive-day-number">${cell.date.getDate()}</span><span class="archive-day-density"><span class="archive-density-track"><i style="width:${density}%"></i></span><small>${duration ? U.formatPracticeDuration(duration) : "No practice"}${takes ? ` · ${takes}` : ""}</small></span>`;
      button.addEventListener("click", () => {
        if (!cell.inMonth) {
          state.month = new Date(cell.date.getFullYear(), cell.date.getMonth(), 1);
          loadMonth();
        }
        selectDay(cell.key);
      });
      calendar.appendChild(button);
    });
    $("#archive-month-title").textContent = monthLabel(state.month);
    const monthPrefix = `${state.month.getFullYear()}-${String(state.month.getMonth() + 1).padStart(2, "0")}-`;
    const monthDays = [...state.calendar.values()].filter((day) => day.date.startsWith(monthPrefix));
    const monthMS = monthDays.reduce((sum, day) => sum + Number(day.totalDurationMs || 0), 0);
    const monthTakes = monthDays.reduce((sum, day) => sum + Number(day.recordingCount || 0), 0);
    $("#archive-month-time").textContent = U.formatPracticeDuration(monthMS, true);
    $("#archive-month-takes").textContent = `${monthTakes} take${monthTakes === 1 ? "" : "s"}`;
  }

  async function selectDay(date) {
    if (!U.parseDateKey(date)) return;
    const requestID = ++state.dayRequest;
    state.selectedDate = date;
    renderCalendar();
    $("#archive-day-title").textContent = fullDateLabel(date);
    $("#archive-day-content").innerHTML = '<p class="archive-empty">Loading this practice day…</p>';
    try {
      let day;
      try {
        day = await api(`/archive/days/${date}`);
      } catch (error) {
        if (error.status !== 404) throw error;
        day = await fallbackDay(date);
      }
      if (requestID !== state.dayRequest) return;
      state.day = day;
      state.playlist = (state.day.recordings || []).filter((recording) => recording.status === "ready");
      if (state.currentRecording) state.currentIndex = state.playlist.findIndex((recording) => recording.id === state.currentRecording.id);
      renderDay();
    } catch (error) {
      if (requestID !== state.dayRequest) return;
      state.day = null;
      state.playlist = [];
      $("#archive-day-time").textContent = "—";
      $("#archive-day-takes").textContent = "—";
      $("#archive-day-content").innerHTML = `<p class="archive-empty">Could not load this day: ${escapeHTML(error.message)}</p>`;
    }
  }

  async function fallbackDay(date) {
    const [sessionResult, recordingResult] = await Promise.all([api("/practice-sessions"), api("/recordings")]);
    const recordings = (recordingResult.recordings || [])
      .filter((recording) => (recording.practiceDate || U.dateKey(new Date(recording.recordedAt))) === date)
      .sort((left, right) => new Date(left.recordedAt) - new Date(right.recordedAt));
    const sessionIDs = new Set(recordings.map((recording) => recording.practiceSessionId).filter(Boolean));
    (sessionResult.sessions || []).forEach((session) => {
      if (U.dateKey(new Date(session.startedAt)) === date) sessionIDs.add(session.id);
    });
    const sessions = await Promise.all([...sessionIDs].map(async (id) => {
      const detail = await api(`/practice-sessions/${id}`);
      const blocks = await api(`/practice-sessions/${id}/blocks?date=${date}`).catch(() => ({ blocks: [] }));
      return { ...detail, blocks: blocks.blocks || [], activities: detail.activities || [] };
    }));
    return {
      date,
      totalDurationMs: sessions.reduce((sum, session) => sum + Number(session.totalDurationMs || 0), 0),
      recordingCount: recordings.length,
      sectionCount: sessions.reduce((sum, session) => sum + session.blocks.length, 0),
      sessions,
      recordings,
    };
  }

  function renderDay() {
    const day = state.day;
    const content = $("#archive-day-content");
    $("#archive-day-time").textContent = U.formatPracticeDuration(day?.totalDurationMs || 0, true);
    $("#archive-day-takes").textContent = String(day?.recordingCount || 0);
    if (!day || (!(day.sessions || []).length && !(day.recordings || []).length)) {
      content.innerHTML = '<p class="archive-empty">No practice was recorded on this day.</p>';
      return;
    }
    const recordingsByBlock = new Map();
    (day.recordings || []).forEach((recording) => {
      const key = recording.practiceBlockId || "";
      if (!recordingsByBlock.has(key)) recordingsByBlock.set(key, []);
      recordingsByBlock.get(key).push(recording);
    });
    const claimedBlocks = new Set();
    const fragments = [];
    (day.sessions || []).forEach((session) => {
      const blocks = session.blocks || [];
      const sessionRecordings = (day.recordings || []).filter((recording) => recording.practiceSessionId === session.id);
      fragments.push(`<article class="archive-session"><header class="archive-session-header"><div><h3>${escapeHTML(session.title)}</h3><span>${escapeHTML(session.status || "session")} · ${U.formatPracticeDuration(session.totalDurationMs || 0, true)} · ${sessionRecordings.length} takes</span></div>${session.summary ? `<p>${escapeHTML(session.summary)}</p>` : ""}</header><div class="archive-section-list">`);
      blocks.forEach((block, index) => {
        claimedBlocks.add(block.id);
        const recordings = recordingsByBlock.get(block.id) || [];
        fragments.push(sectionMarkup(block, recordings, index === 0 && recordings.length > 0));
      });
      const blockTitles = new Set(blocks.map((block) => String(block.title || "").trim().toLowerCase()));
      const splitLegacyFundamentals = blockTitles.has("articulation") && blockTitles.has("flexibility");
      (session.activities || []).filter((activity) => {
        const title = String(activity.title || "").trim().toLowerCase();
        return !blockTitles.has(title) && !(splitLegacyFundamentals && title === "articulation & flexibility");
      }).forEach((activity) => {
        fragments.push(`<article class="archive-section"><div class="archive-section-body" style="padding-top:15px"><strong>${escapeHTML(activity.title)}</strong><p class="archive-section-note">${escapeHTML(activity.notes || "Off-mic practice activity")}</p><span class="archive-section-total"><b>${activity.durationMinutes} min</b>${escapeHTML(activity.category)}</span></div></article>`);
      });
      fragments.push("</div></article>");
    });
    const uncategorized = (day.recordings || []).filter((recording) => !recording.practiceBlockId || !claimedBlocks.has(recording.practiceBlockId));
    if (uncategorized.length) fragments.push(`<article class="archive-session"><header class="archive-session-header"><div><h3>Uncategorized recordings</h3><span>${uncategorized.length} take${uncategorized.length === 1 ? "" : "s"}</span></div></header>${sectionMarkup({ id: "", title: "Open practice", category: "uncategorized", elapsedMs: uncategorized.reduce((sum, item) => sum + Number(item.durationMs || 0), 0), notes: "" }, uncategorized, true)}</article>`);
    content.innerHTML = fragments.join("");
    content.querySelectorAll("[data-archive-recording]").forEach((button) => {
      button.addEventListener("click", () => {
        const recording = (day.recordings || []).find((item) => item.id === button.dataset.archiveRecording);
        if (recording) selectRecording(recording);
      });
    });
    markActiveTake();
  }

  function sectionMarkup(block, recordings, open) {
    const takes = recordings.map((recording) => {
      const status = recording.status === "ready" ? `${recording.mediaKind === "video" ? "Video" : "Audio"} · ${U.formatPlaybackTime(U.durationSeconds(recording.durationMs))}` : recording.status;
      return `<button class="archive-take" type="button" data-archive-recording="${escapeHTML(recording.id)}" ${recording.status === "ready" ? "" : "aria-disabled=\"true\""}><strong>${escapeHTML(U.takeLabel(recording))}</strong><span>${escapeHTML(shortRecordedAt(recording.recordedAt))}</span><small>${escapeHTML(status)}${recording.notes ? ` · ${escapeHTML(recording.notes)}` : ""}</small></button>`;
    }).join("");
    return `<details class="archive-section" ${open ? "open" : ""}><summary><span class="archive-section-title"><strong>${escapeHTML(block.title)}</strong><small>${escapeHTML(block.category || "practice")}</small></span><span class="archive-section-total"><b>${U.formatPracticeDuration(block.elapsedMs || 0, true)}</b>${recordings.length} take${recordings.length === 1 ? "" : "s"}</span></summary><div class="archive-section-body">${block.notes ? `<p class="archive-section-note">${escapeHTML(block.notes)}</p>` : ""}${takes ? `<div class="archive-take-list">${takes}</div>` : '<p class="archive-section-note">No saved takes in this section.</p>'}</div></details>`;
  }

  async function selectRecording(recording, asset = "") {
    if (recording.status !== "ready") return;
    await flushTakeNote();
    state.currentRecording = recording;
    state.currentIndex = state.playlist.findIndex((item) => item.id === recording.id);
    state.currentAsset = asset || (recording.mediaKind === "video" ? "video" : "audio");
    $("#archive-player-empty").hidden = true;
    $("#archive-player").hidden = false;
    $("#archive-player").scrollTop = 0;
    $("#archive-listening-room").classList.add("has-recording");
    $("#archive-player-take").textContent = U.takeLabel(recording);
    $("#archive-player-name").textContent = U.recordingTitle(recording);
    $("#archive-player-date").textContent = `${fullDateLabel(recording.practiceDate || U.dateKey(new Date(recording.recordedAt)))} · ${shortRecordedAt(recording.recordedAt)}`;
    $("#archive-player-note").value = recording.notes || "";
    state.noteDirty = false;
    setNoteStatus("Cloud synced");
    renderPlayerMetadata(recording);
    drawPlayerWaveform(0);
    const assetSwitch = $("#archive-player-asset-switch");
    assetSwitch.hidden = recording.mediaKind !== "video";
    assetSwitch.querySelectorAll("button").forEach((button) => button.classList.toggle("active", button.dataset.archiveAsset === state.currentAsset));
    markActiveTake();
    await loadPlayerAsset(state.currentAsset, true);
  }

  function renderPlayerMetadata(recording) {
    const tune = globalThis.JAZZ_DATA?.repertoire?.find((item) => item.id === recording.tuneId)?.title || "";
    const format = recording.mediaKind === "video"
      ? `${recording.videoWidth || "?"}×${recording.videoHeight || "?"} video + WAV master`
      : (recording.contentType === "audio/wav" ? "Lossless WAV" : recording.contentType || "Audio");
    const rows = [
      ["Section", recording.practiceBlockTitle || "Uncategorized practice"],
      ...(tune ? [["Tune", tune]] : []),
      ["Track", recording.practiceBlockTrack || recording.practiceBlockCategory || "—"],
      ["Format", format],
      ...(recording.sampleRate ? [["Sample rate", `${Math.round(recording.sampleRate / 1000)} kHz · ${recording.channels || 1} channel${recording.channels === 1 ? "" : "s"}`]] : []),
      ["File size", formatBytes(state.currentAsset === "video" ? recording.videoSizeBytes : recording.sizeBytes)],
    ];
    $("#archive-player-metadata").innerHTML = rows.map(([label, value]) => `<div><dt>${escapeHTML(label)}</dt><dd>${escapeHTML(value)}</dd></div>`).join("");
    updateDownloadButton();
  }

  function updateDownloadButton() {
    const button = $("#archive-download-take");
    if (!button) return;
    button.textContent = state.currentAsset === "video" ? "Download video" : "Download lossless audio";
    button.disabled = !state.currentRecording;
  }

  async function loadPlayerAsset(asset, autoplay, retry = 0) {
    const recording = state.currentRecording;
    if (!recording) return;
    const requestID = ++state.playbackRequest;
    setPlayerState("Loading");
    try {
      const result = await api(`/recordings/${recording.id}/playback-url?asset=${encodeURIComponent(asset)}`, { method: "POST", body: "{}" });
      if (requestID !== state.playbackRequest) return;
      state.media?.pause();
      const media = document.createElement(result.contentType?.startsWith("video/") ? "video" : "audio");
      media.preload = "metadata";
      media.playsInline = true;
      media.src = result.url;
      if (media.tagName === "VIDEO") media.setAttribute("aria-label", `${U.recordingTitle(recording)} video`);
      $("#archive-media-host").replaceChildren(media);
      state.media = media;
      state.currentAsset = result.asset || asset;
      state.playbackExpiresAt = new Date(result.expiresAt || 0).valueOf() || 0;
      state.expectedDuration = U.durationSeconds(result.durationMs || recording.durationMs);
      const seek = $("#archive-player-seek");
      seek.max = String(state.expectedDuration);
      seek.value = "0";
      $("#archive-player-current").textContent = U.formatPlaybackTime(0);
      $("#archive-player-duration").textContent = U.formatPlaybackTime(state.expectedDuration);
      drawPlayerWaveform(0);
      wireMedia(media, state.currentAsset, retry);
      $("#archive-player-asset-switch").querySelectorAll("button").forEach((button) => button.classList.toggle("active", button.dataset.archiveAsset === state.currentAsset));
      renderPlayerMetadata(recording);
      if (autoplay) await media.play().catch(() => setPlayerState("Ready"));
      else setPlayerState("Ready");
    } catch (error) {
      if (requestID !== state.playbackRequest) return;
      setPlayerState(`Unavailable · ${error.message}`);
    }
  }

  function wireMedia(media, asset, retry) {
    const syncNativeDuration = () => {
      const nativeDuration = Number(media.duration);
      if (!Number.isFinite(nativeDuration) || nativeDuration <= 0) return;
      state.expectedDuration = nativeDuration;
      $("#archive-player-seek").max = String(nativeDuration);
      $("#archive-player-duration").textContent = U.formatPlaybackTime(nativeDuration);
    };
    const update = () => {
      const current = Math.min(state.expectedDuration, Math.max(0, Number(media.currentTime || 0)));
      $("#archive-player-seek").value = String(current);
      $("#archive-player-current").textContent = U.formatPlaybackTime(current);
      drawPlayerWaveform(current);
      $("#archive-player-toggle").textContent = media.paused || media.ended ? "▶" : "Ⅱ";
      $("#archive-player-toggle").setAttribute("aria-label", media.paused || media.ended ? "Play" : "Pause");
      if (!media.paused && !media.ended) setPlayerState("Listening");
      else if (media.ended) setPlayerState("Finished");
    };
    ["play", "pause", "timeupdate", "seeking", "seeked", "ended", "volumechange"].forEach((event) => media.addEventListener(event, update));
    ["loadedmetadata", "durationchange"].forEach((event) => media.addEventListener(event, () => {
      syncNativeDuration();
      update();
    }));
    media.addEventListener("error", () => {
      if (media !== state.media) return;
      if (retry < 1 && state.currentRecording) {
        setPlayerState("Refreshing private link");
        loadPlayerAsset(asset, false, retry + 1);
      } else {
        setPlayerState("Playback interrupted");
      }
    });
    syncNativeDuration();
    update();
  }

  function setPlayerState(message) {
    $("#archive-player-state").textContent = message;
  }

  function markActiveTake() {
    document.querySelectorAll("[data-archive-recording]").forEach((button) => button.classList.toggle("active", button.dataset.archiveRecording === state.currentRecording?.id));
    $("#archive-player-previous").disabled = state.currentIndex <= 0;
    $("#archive-player-next").disabled = state.currentIndex < 0 || state.currentIndex >= state.playlist.length - 1;
  }

  function setNoteStatus(message, tone = "") {
    const element = $("#archive-player-note-status");
    element.textContent = message;
    element.dataset.tone = tone;
  }

  async function flushTakeNote() {
    clearTimeout(state.noteTimer);
    state.noteTimer = null;
    if (!state.noteDirty || !state.currentRecording) return;
    const recording = state.currentRecording;
    const notes = $("#archive-player-note").value.trim();
    setNoteStatus("Saving…", "saving");
    try {
      const result = await api(`/recordings/${recording.id}`, { method: "PATCH", body: JSON.stringify({ notes }) });
      recording.notes = result.notes || "";
      state.noteDirty = false;
      setNoteStatus("Cloud synced", "saved");
      renderDay();
    } catch (error) {
      setNoteStatus(`Not saved · ${error.message}`, "error");
    }
  }

  async function deleteCurrentTake() {
    const recording = state.currentRecording;
    if (!recording || !confirm(`Delete ${U.takeLabel(recording)} from ${U.recordingTitle(recording)}? This cannot be undone.`)) return;
    await api(`/recordings/${recording.id}`, { method: "DELETE" });
    closePlayer();
    await Promise.all([loadMonth(), selectDay(state.selectedDate)]);
  }

  function closePlayer() {
    flushTakeNote();
    state.media?.pause();
    state.media = null;
    state.currentRecording = null;
    state.currentIndex = -1;
    state.playbackRequest++;
    $("#archive-media-host").replaceChildren();
    $("#archive-player").hidden = true;
    $("#archive-player-empty").hidden = false;
    $("#archive-listening-room").classList.remove("has-recording");
    setPlayerState("Listening room");
    markActiveTake();
  }

  function wireArchiveControls() {
    $("#archive-previous-month").addEventListener("click", () => changeMonth(-1));
    $("#archive-next-month").addEventListener("click", () => changeMonth(1));
    $("#archive-today-button").addEventListener("click", () => {
      const now = new Date();
      state.month = new Date(now.getFullYear(), now.getMonth(), 1);
      state.selectedDate = U.dateKey(now);
      loadMonth();
      selectDay(state.selectedDate);
    });
    $("#archive-player-close").addEventListener("click", closePlayer);
    $("#archive-player-toggle").addEventListener("click", () => {
      if (state.currentRecording && state.playbackExpiresAt && Date.now() >= state.playbackExpiresAt - 15000) loadPlayerAsset(state.currentAsset, true);
      else if (!state.media && state.currentRecording) loadPlayerAsset(state.currentAsset, true);
      else if (state.media?.paused || state.media?.ended) state.media.play().catch(() => {});
      else state.media?.pause();
    });
    $("#archive-player-seek").addEventListener("input", (event) => {
      if (!state.media) return;
      state.media.currentTime = Math.min(state.expectedDuration, Math.max(0, Number(event.target.value || 0)));
    });
    $("#archive-player-back").addEventListener("click", () => seekBy(-10));
    $("#archive-player-forward").addEventListener("click", () => seekBy(10));
    $("#archive-player-previous").addEventListener("click", () => selectPlaylistIndex(state.currentIndex - 1));
    $("#archive-player-next").addEventListener("click", () => selectPlaylistIndex(state.currentIndex + 1));
    $("#archive-player-asset-switch").addEventListener("click", (event) => {
      const button = event.target.closest("[data-archive-asset]");
      if (button && button.dataset.archiveAsset !== state.currentAsset) loadPlayerAsset(button.dataset.archiveAsset, true);
    });
    $("#archive-player-note").addEventListener("input", () => {
      state.noteDirty = true;
      setNoteStatus("Waiting to sync", "saving");
      clearTimeout(state.noteTimer);
      state.noteTimer = setTimeout(flushTakeNote, 700);
    });
    $("#archive-player-note").addEventListener("blur", flushTakeNote);
    $("#archive-download-take").addEventListener("click", () => {
      if (!state.currentRecording) return;
      globalThis.JazzRecording?.download(state.currentRecording.id, state.currentAsset, $("#archive-download-take"))
        .catch((error) => setPlayerState(`Download failed · ${error.message}`));
    });
    $("#archive-share-take").addEventListener("click", () => {
      if (!state.currentRecording) return;
      globalThis.JazzRecording?.share(state.currentRecording.id, state.currentAsset, $("#archive-share-take"))
        .then(() => setPlayerState("Permanent share link copied"))
        .catch((error) => setPlayerState(`Share failed · ${error.message}`));
    });
    $("#archive-delete-take").addEventListener("click", () => deleteCurrentTake().catch((error) => setPlayerState(`Delete failed · ${error.message}`)));
    document.addEventListener("keydown", (event) => {
      if (visibleView() !== "archive" || event.code !== "Space" || event.target.matches("input,textarea,button,select")) return;
      event.preventDefault();
      $("#archive-player-toggle").click();
    });
    window.addEventListener("resize", () => drawPlayerWaveform(Number(state.media?.currentTime || 0)));
  }

  function changeMonth(delta) {
    state.month = new Date(state.month.getFullYear(), state.month.getMonth() + delta, 1);
    loadMonth();
  }

  function seekBy(seconds) {
    if (!state.media) return;
    state.media.currentTime = Math.min(state.expectedDuration, Math.max(0, Number(state.media.currentTime || 0) + seconds));
  }

  function selectPlaylistIndex(index) {
    if (index < 0 || index >= state.playlist.length) return;
    selectRecording(state.playlist[index]);
  }

  window.addEventListener("hashchange", route);
  window.addEventListener("resize", resizeEffectsFrame);
  $("#effects-frame")?.addEventListener("load", watchEffectsFrameSize);
  route();
})();
