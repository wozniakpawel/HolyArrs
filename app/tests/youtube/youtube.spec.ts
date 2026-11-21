import { expect, test, type Browser, type Page } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { api } from "../helpers/api.ts";
import { axe } from "../helpers/axe.ts";
import { tmp } from "../helpers/data-folder.ts";
import { owner, signIn } from "../helpers/helpers.ts";

// The YouTube library with a fake yt-dlp (tests/fakes/yt-dlp.mjs): the owner
// sets the folder, downloads one video and watches part of it, follows a
// channel; a member without the YouTube permission sees none of it.

test.describe.configure({ mode: "serial" });

const folder = join(tmp, "youtube");
const videoLink = "https://www.youtube.com/watch?v=dQw4w9WgXcQ";
const videoTitle = "Never Gonna Give You Up";
const channelLink = "https://www.youtube.com/@veritasium";
const channelName = "Veritasium";
const memberPw = `e2e-${Math.random().toString(36).slice(2)}-member`;

async function fresh(browser: Browser): Promise<Page> {
  return (await browser.newContext()).newPage();
}

let own: Page;
let videoPage = "";

test.beforeAll(async ({ browser }) => {
  own = await fresh(browser);
  await signIn(own, owner());
});

test("the owner sets the YouTube folder in Settings > YouTube", async () => {
  mkdirSync(folder, { recursive: true });
  await own.goto("/settings/youtube");
  await expect(
    own.getByRole("heading", { level: 1, name: "YouTube" }),
  ).toBeVisible();
  await expect(own.getByTestId("yt-ytdlp")).toContainText("Found at");
  await expect(own.getByTestId("yt-ytdlp")).toContainText("Version 2026.10.26");
  await expect(own.getByTestId("yt-ffmpeg")).toContainText("Found at");
  await own.getByLabel("YouTube folder").fill(folder);
  // The test machine's disk may have less than the default 10 GB free.
  await own.getByLabel("Free space to keep (GB)").fill("1");
  await own
    .getByTestId("savebar")
    .getByRole("button", { name: "Save" })
    .click();
  await expect(own.getByText("YouTube settings saved.")).toBeVisible();
  await own.reload();
  await expect(own.getByLabel("YouTube folder")).toHaveValue(folder);
});

test("a pasted video link downloads, and its card goes from downloading to done", async () => {
  await own.goto("/youtube");
  await expect(
    own
      .getByRole("navigation", { name: "Main" })
      .first()
      .getByRole("link", { name: "YouTube", exact: true }),
  ).toBeVisible();
  await expect(own.getByTestId("youtube-no-folder")).toHaveCount(0);
  await own.getByLabel("YouTube link").fill(videoLink);
  await own
    .getByTestId("youtube-add")
    .getByRole("button", { name: "Add" })
    .click();
  const confirm = own.getByTestId("youtube-confirm");
  await expect(
    confirm.getByRole("heading", { name: videoTitle }),
  ).toBeVisible();
  await expect(confirm).toContainText("Rick Astley");
  await confirm.getByRole("button", { name: "Download", exact: true }).click();
  const card = own.getByTestId("youtube-video").filter({ hasText: videoTitle });
  await expect(card).toHaveAttribute("data-state", "downloading", {
    timeout: 30_000,
  });
  await expect(card).toContainText(/Downloading \d+%/);
  await expect(card).toHaveAttribute("data-state", "done", { timeout: 30_000 });
  await expect(card.getByRole("link", { name: videoTitle })).toBeVisible();
  await axe(own, "youtube page");
});

test("the video plays from a file that answers ranges, and picks up where it was left", async () => {
  await own
    .getByTestId("youtube-video")
    .filter({ hasText: videoTitle })
    .getByRole("link", { name: videoTitle })
    .click();
  await expect(own).toHaveURL(/\/youtube\/\d+$/);
  videoPage = new URL(own.url()).pathname;
  const player = own.getByTestId("video-player");
  await expect(player).toBeVisible();
  await expect(own.getByRole("link", { name: "Save to device" })).toBeVisible();
  const src = await player.evaluate(
    (v: HTMLVideoElement) => v.currentSrc || v.src,
  );
  const ranged = await own.request.get(src, {
    headers: { Range: "bytes=0-99" },
  });
  expect(ranged.status()).toBe(206);
  expect((await ranged.body()).length).toBe(100);
  await axe(own, "video page");

  // Play two seconds, then pause; the pause saves the position.
  await expect
    .poll(() => player.evaluate((v: HTMLVideoElement) => v.readyState), {
      timeout: 15_000,
    })
    .toBeGreaterThanOrEqual(1);
  const saved = own.waitForResponse(
    (r) =>
      /\/api\/v1\/youtube\/videos\/\d+\/position$/.test(r.url()) &&
      r.request().method() === "PUT" &&
      r.ok(),
  );
  await player.evaluate(async (v: HTMLVideoElement) => {
    v.muted = true;
    await v.play();
  });
  await expect
    .poll(() => player.evaluate((v: HTMLVideoElement) => v.currentTime), {
      timeout: 15_000,
    })
    .toBeGreaterThanOrEqual(2);
  await player.evaluate((v: HTMLVideoElement) => v.pause());
  await saved;

  // Leave, come back: the player starts where it stopped.
  await own.getByRole("link", { name: "All videos" }).click();
  await expect(own).toHaveURL(/\/youtube$/);
  await own
    .getByTestId("youtube-video")
    .filter({ hasText: videoTitle })
    .getByRole("link", { name: videoTitle })
    .click();
  await expect(own).toHaveURL(new RegExp(`${videoPage}$`));
  await expect
    .poll(
      () =>
        own
          .getByTestId("video-player")
          .evaluate((v: HTMLVideoElement) => v.currentTime),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(0);
  const id = videoPage.split("/").at(-1);
  const row = (await (
    await own.request.get(`/api/v1/youtube/videos/${id}`)
  ).json()) as { position_seconds: number };
  expect(row.position_seconds).toBeGreaterThan(0);
});

test("following a channel names it on the Following line", async () => {
  await own.goto("/youtube");
  await own.getByLabel("YouTube link").fill(channelLink);
  await own
    .getByTestId("youtube-add")
    .getByRole("button", { name: "Add" })
    .click();
  const confirm = own.getByTestId("youtube-confirm");
  await expect(
    confirm.getByRole("heading", { name: channelName }),
  ).toBeVisible();
  await confirm.getByRole("button", { name: "Follow", exact: true }).click();
  const following = own.getByTestId("youtube-following");
  await expect(following).toContainText("Following:");
  await expect(
    following.getByRole("button", { name: channelName }),
  ).toBeVisible();
});

test("a member without the YouTube permission has no YouTube item and is refused by the API", async ({
  browser,
}) => {
  const call = await api(own);
  const made = await call("POST", "/users/invites", {
    role: "member",
    note: "Yara",
  });
  expect(made.status()).toBe(201);
  const { path } = (await made.json()) as { path: string };

  const yara = await fresh(browser);
  await yara.goto(path);
  await expect(
    yara.getByRole("heading", { name: "Join HolyArrs" }),
  ).toBeVisible();
  await yara.getByLabel("Name").fill("yara");
  await yara.getByLabel("Password", { exact: true }).fill(memberPw);
  await yara.getByLabel("Confirm password").fill(memberPw);
  await yara.getByRole("button", { name: "Join and sign in" }).click();
  await expect(yara).toHaveURL(/\/$/);

  const nav = yara.getByRole("navigation", { name: "Main" }).first();
  await expect(
    nav.getByRole("link", { name: "Calendar", exact: true }),
  ).toBeVisible();
  await expect(
    nav.getByRole("link", { name: "YouTube", exact: true }),
  ).toHaveCount(0);
  expect((await yara.request.get("/api/v1/youtube/videos")).status()).toBe(403);
  expect((await yara.request.get("/api/v1/youtube/status")).status()).toBe(403);
  await yara.context().close();
});
