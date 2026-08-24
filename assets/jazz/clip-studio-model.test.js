const test = require("node:test");
const assert = require("node:assert/strict");
const model = require("./clip-studio-model.js");

function clip(id, startMs = 0, endMs = 1000) {
  return { id, recordingId: `recording-${id}`, startMs, endMs };
}

test("output clips are deep copies and preserve their added boundaries", () => {
  const source = { ...clip("one", 1000, 4000), liked: true };
  const project = model.addClip(model.createProject("2026-08-22"), source);
  source.startMs = 2000;
  source.liked = false;
  assert.equal(project.clips[0].startMs, 1000);
  assert.equal(project.clips[0].liked, true);
});

test("candidate likes map to the persistent kept review state", () => {
  assert.equal(model.candidateIsLiked({ reviewStatus: "kept" }), true);
  assert.equal(model.candidateIsLiked({ reviewStatus: "suggested" }), false);
  assert.equal(model.nextLikeStatus({ reviewStatus: "kept" }), "suggested");
  assert.equal(model.nextLikeStatus({ reviewStatus: "suggested" }), "kept");
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

test("manual source clips default to ten seconds centered on the click", () => {
  assert.deepEqual(model.placeDefaultSourceClip(30000, 60000, []), { startMs: 25000, endMs: 35000 });
  assert.deepEqual(model.placeDefaultSourceClip(2000, 60000, []), { startMs: 0, endMs: 10000 });
  assert.deepEqual(model.placeDefaultSourceClip(59000, 60000, []), { startMs: 50000, endMs: 60000 });
});

test("manual source clips push into the nearest open ten-second gap", () => {
  const existing = [{ startMs: 10000, endMs: 20000, reviewStatus: "suggested" }];
  assert.deepEqual(model.placeDefaultSourceClip(8000, 60000, existing), { startMs: 0, endMs: 10000 });
  assert.deepEqual(model.placeDefaultSourceClip(22000, 60000, existing), { startMs: 20000, endMs: 30000 });
});

test("manual source clip clicks inside clips or on their boundaries are no-ops", () => {
  const existing = [{ startMs: 10000, endMs: 20000, reviewStatus: "kept" }];
  assert.equal(model.placeDefaultSourceClip(10000, 60000, existing), null);
  assert.equal(model.placeDefaultSourceClip(15000, 60000, existing), null);
  assert.equal(model.placeDefaultSourceClip(20000, 60000, existing), null);
});

test("manual source clip placement ignores rejected clips and returns null without enough room", () => {
  assert.deepEqual(model.placeDefaultSourceClip(15000, 30000, [{ startMs: 10000, endMs: 20000, reviewStatus: "rejected" }]), { startMs: 10000, endMs: 20000 });
  assert.equal(model.placeDefaultSourceClip(25000, 30000, [{ startMs: 0, endMs: 12000 }, { startMs: 18000, endMs: 30000 }]), null);
  assert.equal(model.placeDefaultSourceClip(200, 400, []), null);
});
