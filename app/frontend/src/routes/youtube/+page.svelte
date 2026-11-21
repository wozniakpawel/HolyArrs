<script lang="ts">
	// The person's own YouTube videos: paste a link, follow playlists and channels, and watch what is downloaded.
	import { untrack } from 'svelte';
	import { t } from '#lib/core/i18n/i18n.svelte.ts';
	import { message } from '#lib/core/api.ts';
	import { bytes } from '#lib/core/format.ts';
	import { live } from '#lib/core/live.svelte.ts';
	import { to } from '#lib/core/paths.ts';
	import { session } from '#lib/core/session.svelte.ts';
	import { toasts } from '#lib/core/toasts.svelte.ts';
	import { ask } from '#lib/ui/ask.svelte.ts';
	import Button from '#lib/ui/Button.svelte';
	import Chip from '#lib/ui/Chip.svelte';
	import EmptyState from '#lib/ui/EmptyState.svelte';
	import Icon from '#lib/ui/Icon.svelte';
	import LoadError from '#lib/ui/LoadError.svelte';
	import PageHeader from '#lib/ui/PageHeader.svelte';
	import PageLoader from '#lib/ui/PageLoader.svelte';
	import {
		AddLink,
		ConfirmCard,
		DownloadStrip,
		FollowPanel,
		NeedsPrograms,
		VideoCard,
		applyLive,
		channelsOf,
		filterVideos,
		shownVideos,
		ytFollows,
		ytRemove,
		ytRetry,
		ytStatus,
		ytVideos,
		type YtFollow,
		type YtLive,
		type YtProbe,
		type YtStatus,
		type YtVideo
	} from '#lib/youtube/index.ts';

	// The layout sends anyone else away; nothing of the page draws for them meanwhile.
	const mayUse = $derived(session.can('youtube') && !session.kid && !session.held('youtube'));

	let status = $state<YtStatus | null>(null);
	let videos = $state<YtVideo[]>([]);
	let follows = $state<YtFollow[]>([]);
	let loaded = $state(false);
	let error = $state('');

	let probe = $state<{ probe: YtProbe; url: string } | null>(null);
	let pick = $state('');
	let unwatched = $state(false);
	let manageOpen = $state(false);
	let focus = $state<number | null>(null);

	const shown = $derived(shownVideos(videos));
	const channels = $derived(channelsOf(shown));
	const filtered = $derived(
		filterVideos(shown, {
			followId: pick.startsWith('f:') ? Number(pick.slice(2)) : undefined,
			channelId: pick.startsWith('c:') ? pick.slice(2) : undefined,
			unwatched
		})
	);

	async function load() {
		try {
			const [v, f] = await Promise.all([ytVideos(), ytFollows()]);
			videos = v;
			follows = f;
			error = '';
		} catch (e) {
			error = message(e);
		} finally {
			loaded = true;
		}
		try {
			status = await ytStatus();
		} catch {
			// The notes above the grid wait for the next load.
		}
	}

	// A video this page does not hold yet (a check queued it), or one just finished, comes in with a fresh list.
	let soon: ReturnType<typeof setTimeout> | undefined;
	function reloadSoon() {
		clearTimeout(soon);
		soon = setTimeout(() => void load(), 800);
	}

	// Starts once the session says the person may use the page.
	$effect(() => {
		if (!mayUse) return;
		untrack(() => void load());
		const offs = [
			live.on('youtube', (data) => {
				const ev = data as YtLive;
				if (typeof ev?.id !== 'number') return;
				const known = videos.some((v) => v.id === ev.id);
				if (known) videos = applyLive(videos, ev);
				if (!known || ev.state === 'done') reloadSoon();
			}),
			live.on('resync', () => reloadSoon())
		];
		return () => {
			offs.forEach((off) => off());
			clearTimeout(soon);
		};
	});

	function added(v: YtVideo) {
		videos = [v, ...videos.filter((x) => x.id !== v.id)];
		probe = null;
		toasts.push(t('youtube.added'), 'ok');
	}

	function followed(f: YtFollow) {
		follows = [...follows.filter((x) => x.id !== f.id), f];
		probe = null;
		toasts.push(t('youtube.followed', { name: f.title }), 'ok');
		reloadSoon();
	}

	function showFollow(id: number) {
		focus = id;
		manageOpen = true;
	}

	function showVideosOf(id: number) {
		pick = `f:${id}`;
		manageOpen = false;
	}

	function changed(f: YtFollow) {
		follows = follows.map((x) => (x.id === f.id ? f : x));
	}

	function unfollowed(id: number, files: 'keep' | 'delete') {
		follows = follows.filter((x) => x.id !== id);
		videos = files === 'delete' ? videos.filter((v) => v.follow_id !== id) : videos.map((v) => (v.follow_id === id ? { ...v, follow_id: null } : v));
		if (pick === `f:${id}`) pick = '';
	}

	async function retry(v: YtVideo) {
		try {
			const fresh = await ytRetry(v.id);
			videos = videos.map((x) => (x.id === fresh.id ? fresh : x));
		} catch (e) {
			toasts.push(t('youtube.retryFailed'), 'fail', { body: message(e) });
		}
	}

	async function remove(v: YtVideo) {
		if (!(await ask(t('youtube.card.removeAsk', { title: v.title || v.video_id }), { yes: t('youtube.card.remove'), danger: true }))) return;
		try {
			await ytRemove(v.id);
			videos = videos.map((x) => (x.id === v.id ? { ...x, state: 'removed' } : x));
			toasts.push(t('youtube.card.removed'), 'ok');
		} catch (e) {
			toasts.push(t('youtube.card.removeFailed'), 'fail', { body: message(e) });
		}
	}
</script>

<svelte:head><title>{t('youtube.title')} · HolyArrs</title></svelte:head>

{#if mayUse}
	<PageHeader title={t('youtube.title')} />

	<div class="page">
		{#if status && status.missing.length > 0}
			<NeedsPrograms missing={status.missing} />
		{:else if status && !status.folder_set && session.s.role === 'owner'}
			<div class="notice warn" role="status" data-testid="youtube-no-folder">
				<Icon name="folder" size={18} />
				<div class="grow">
					<p class="lead">{t('youtube.folder.title')}</p>
					<p class="small">{t('youtube.folder.text')}</p>
				</div>
				<Button size="sm" href={to('/settings/youtube')}>{t('youtube.folder.open')}</Button>
			</div>
		{/if}
		{#if status?.low_space}
			<p class="notice warn" role="status" data-testid="youtube-low-space">
				<Icon name="alert" size={18} /><span>{t('youtube.lowSpace', { floor: bytes(status.floor_bytes) })}</span>
			</p>
		{/if}

		<AddLink disabled={!!status && status.missing.length > 0} onprobed={(p, url) => (probe = { probe: p, url })} />
		{#if probe}
			{#key probe}
				<ConfirmCard probe={probe.probe} url={probe.url} onvideo={added} onfollow={followed} onshowfollow={showFollow} oncancel={() => (probe = null)} />
			{/key}
		{/if}

		{#if follows.length > 0}
			<div class="following" data-testid="youtube-following">
				<span class="small muted">{t('youtube.following')}</span>
				{#each follows as f, i (f.id)}
					<button type="button" class="link small" class:paused={!f.enabled} aria-pressed={pick === `f:${f.id}`} onclick={() => (pick = pick === `f:${f.id}` ? '' : `f:${f.id}`)}>{f.title}</button>{#if i < follows.length - 1}<span class="small muted" aria-hidden="true">·</span>{/if}
				{/each}
				<Button size="sm" variant="plain" onclick={() => { focus = null; manageOpen = true; }} data-testid="youtube-manage">{t('youtube.manage')}</Button>
			</div>
		{/if}

		<DownloadStrip videos={shown} />

		<PageLoader loading={!loaded}>
			{#if error && videos.length === 0}
				<LoadError hasData={false} message={error} onretry={load} />
			{:else}
				{#if error}<LoadError hasData={true} message={error} onretry={load} />{/if}
				{#if shown.length > 0}
					<div class="filters">
						<label class="sel">
							<span class="small muted">{t('youtube.filter.label')}</span>
							<select class="input" bind:value={pick} data-testid="youtube-filter">
								<option value="">{t('youtube.filter.everything')}</option>
								{#if follows.length > 0}
									<optgroup label={t('youtube.filter.follows')}>
										{#each follows as f (f.id)}<option value="f:{f.id}">{f.title}</option>{/each}
									</optgroup>
								{/if}
								{#if channels.length > 0}
									<optgroup label={t('youtube.filter.channels')}>
										{#each channels as c (c.id)}<option value="c:{c.id}">{c.name}</option>{/each}
									</optgroup>
								{/if}
							</select>
						</label>
						<Chip label={t('youtube.filter.unwatched')} selected={unwatched} onclick={() => (unwatched = !unwatched)} />
					</div>
				{/if}
				{#if shown.length === 0}
					<EmptyState icon="play" title={t('youtube.empty.title')} text={t('youtube.empty.text')} />
				{:else if filtered.length === 0}
					<EmptyState compact icon="search" title={t('youtube.empty.filtered')} />
				{:else}
					<div class="grid" data-testid="youtube-grid">
						{#each filtered as v (v.id)}
							<VideoCard video={v} onretry={retry} onremove={remove} />
						{/each}
					</div>
				{/if}
			{/if}
		</PageLoader>
	</div>

	<FollowPanel bind:open={manageOpen} {follows} {focus} onchanged={changed} onunfollowed={unfollowed} onshow={showVideosOf} />
{/if}

<style>
	.page { display: grid; gap: var(--s-4); }
	.notice p { margin: 0; }
	.notice .lead { font-weight: 600; }
	.following { display: flex; align-items: center; flex-wrap: wrap; gap: var(--s-1) var(--s-2); }
	.link { border: 0; background: none; padding: 0; color: var(--text); cursor: pointer; font: inherit; }
	.link:hover { text-decoration: underline; }
	.link[aria-pressed='true'] { color: var(--accent); font-weight: 600; }
	.link.paused { color: var(--text-muted); }
	.filters { display: flex; align-items: center; flex-wrap: wrap; gap: var(--s-3); }
	.sel { display: flex; align-items: center; gap: var(--s-2); }
	.sel .input { width: auto; max-width: 100%; }
	.grid { display: grid; gap: var(--s-5) var(--s-4); grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); }
</style>
