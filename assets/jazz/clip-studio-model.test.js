const test = require("node:test");
const assert = require("node:assert/strict");
const model = require("./clip-studio-model.js");

function clip(id, startMs = 0, endMs = 1000) {
  return { id, recordingId: `recording-${id}`, startMs, endMs };
}

test("output clips are deep copies and preserve their added boundaries", () => {
  const source = clip("one", 1000, 4000);
  const project = model.addClip(model.createProject("2026-08-22"), source);
  source.startMs = 2000;
  assert.equal(project.clips[0].startMs, 1000);
});

test("output clips can be reordered and removed", () => {
  let project = model.createProject("2026-08-22");
  project = model.addClip(project, clip("one"));
  project = model.addClip(project, clip("two"));
  project = model.moveClip(project, 0, 1);
  assert.deepEqual(project.clips.map((item) => item.id), ["two", "one"]);
  project = model.removeClip(project, "two");
  assert.deepEqual(project.clips.map((item) => item.id), ["one"]);
});

test("render payload contains only server-authoritative clip identifiers and boundaries", () => {
  const project = model.addClip({ ...model.createProject("2026-08-22"), title: "My Movie" }, clip("one", 1200, 4500));
  assert.deepEqual(model.renderPayload(project), { title: "My Movie", clips: [{ recordingId: "recording-one", startMs: 1200, endMs: 4500 }] });
});

test("output timeline enforces the ten minute render ceiling", () => {
  const project = model.addClip(model.createProject("2026-08-22"), clip("one", 0, model.MAX_DURATION_MS));
  assert.throws(() => model.addClip(project, clip("two")), /ten minutes/);
});
