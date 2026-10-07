(() => {
    'use strict';

    const LIVE = {
        maxLatency: 8,
        targetLatency: 2.5,
        hlsMaxLatencyFragments: 3,
        hlsSyncFragments: 2,
        stallProbeDelay: 1200,
        stallProgress: 0.25,
        pausedGrace: 1.5,
        maxRetries: 10,
    };

    const state = {
        channels: [],
        filteredChannels: [],
        selectedId: null,
        currentHash: '',
        hashInitialized: false,
        catalogReady: false,
        pendingHash: '',
        hls: null,
        mpegts: null,
        videoLoadedHandler: null,
        videoErrorHandler: null,
        playbackToken: 0,
        retryCount: 0,
        channelsController: null,
        updateInFlight: false,
        statusInFlight: false,
        searchTimer: null,
        toastTimer: null,
        stallTimer: null,
        liveWatcher: null,
        connection: null,
        segmentAlerts: 0,
        lastProgressTime: 0,
        pausedAt: 0,
        autoPaused: false,
        loadingSuspended: false,
        tearingDown: false,
        showFailureStatus: false,
        failureNotified: false,
    };

    const elements = {};

    document.addEventListener('DOMContentLoaded', init);

    function init() {
        elements.sidebar = document.getElementById('sidebar');
        elements.overlay = document.getElementById('overlay');
        elements.menuToggle = document.getElementById('menuToggle');
        elements.sidebarClose = document.getElementById('sidebarClose');
        elements.channelList = document.getElementById('channelList');
        elements.searchInput = document.getElementById('searchInput');
        elements.updateBtn = document.getElementById('updateBtn');
        elements.updateStatus = document.getElementById('updateStatus');
        elements.channelCount = document.getElementById('channelCount');
        elements.visibleCount = document.getElementById('visibleCount');
        elements.connectionStatus = document.getElementById('connectionStatus');
        elements.channelName = document.getElementById('channelName');
        elements.channelInfo = document.getElementById('channelInfo');
        elements.channelInfoText = document.getElementById('channelInfoText');
        elements.player = document.getElementById('player');
        elements.playerEmpty = document.getElementById('playerEmpty');
        elements.openChannelsBtn = document.getElementById('openChannelsBtn');
        elements.toast = document.getElementById('toast');

        elements.updateBtn.addEventListener('click', updateChannels);
        elements.searchInput.addEventListener('input', onSearchInput);
        elements.searchInput.addEventListener('keydown', event => {
            if (event.key === 'Escape') {
                elements.searchInput.value = '';
                applyFilter('');
                elements.searchInput.blur();
            }
        });
        elements.channelList.addEventListener('click', onChannelListClick);
        elements.menuToggle.addEventListener('click', () => toggleSidebar());
        elements.sidebarClose.addEventListener('click', closeSidebar);
        elements.overlay.addEventListener('click', closeSidebar);
        elements.openChannelsBtn.addEventListener('click', () => toggleSidebar(true));
        elements.player.addEventListener('pause', onPlayerPause);
        elements.player.addEventListener('play', onPlayerPlay);
        elements.player.addEventListener('seeking', () => {
            window.clearTimeout(state.stallTimer);
            state.stallTimer = null;
        });
        document.addEventListener('visibilitychange', onVisibilityChange);
        document.addEventListener('keydown', onDocumentKeydown);

        renderChannels([]);
        loadChannels();
        pollStatus();
        window.setInterval(pollStatus, 30000);
    }

    async function fetchJSON(resource, options = {}) {
        const response = await fetch(resource, {
            ...options,
            headers: {
                Accept: 'application/json',
                ...(options.headers || {}),
            },
        });
        if (!response.ok) {
            throw new Error(`HTTP ${response.status}`);
        }
        return response.json();
    }

    async function loadChannels() {
        if (state.channelsController) {
            state.channelsController.abort();
        }
        const controller = new AbortController();
        state.channelsController = controller;
        setListState('正在加载频道...', 'loading');
        setUpdateStatus('正在同步频道', 'normal');

        try {
            const payload = await fetchJSON('/api/channels', { signal: controller.signal });
            if (controller.signal.aborted) return;
            if (!Array.isArray(payload)) {
                throw new Error('频道数据格式错误');
            }

            state.channels = payload.map(normalizeChannel).filter(channel => channel.id);
            streamTypeCache.clear();
            state.catalogReady = true;
            state.currentHash = state.hashInitialized ? state.currentHash : state.pendingHash;
            const selectedStillExists = state.channels.some(channel => channel.id === state.selectedId);
            if (!selectedStillExists) {
                state.selectedId = null;
                stopPlayback();
            }
            applyFilter(elements.searchInput.value);
            updateCounts(state.filteredChannels.length);
            setUpdateStatus(formatCatalogStatus(state.channels.length), 'normal');
        } catch (error) {
            if (error.name === 'AbortError') return;
            state.catalogReady = false;
            setListState('加载失败，请检查服务或配置', 'error');
            setUpdateStatus('频道加载失败', 'error');
            setConnectionStatus('离线', false);
            showToast('频道列表加载失败', 'error');
        } finally {
            if (state.channelsController === controller) {
                state.channelsController = null;
            }
        }
    }

    function normalizeChannel(channel) {
        const id = typeof channel?.id === 'string' ? channel.id.trim() : '';
        const name = typeof channel?.name === 'string' && channel.name.trim()
            ? channel.name.trim()
            : id || '未命名频道';
        const group = typeof channel?.group === 'string' && channel.group.trim()
            ? channel.group.trim()
            : '未分类';
        const logo = typeof channel?.logo === 'string' ? channel.logo.trim() : '';
        const playlistURL = typeof channel?.playlist_url === 'string'
            ? channel.playlist_url.trim()
            : '';
        const streamURL = typeof channel?.stream_url === 'string'
            ? channel.stream_url.trim()
            : '';
        const typeURL = typeof channel?.type_url === 'string'
            ? channel.type_url.trim()
            : '';
        const streamType = typeof channel?.stream_type === 'string'
            ? channel.stream_type.trim().toLowerCase()
            : 'auto';
        return { id, name, group, logo, playlistURL, streamURL, typeURL, streamType };
    }

    function renderChannels(channels) {
        const fragment = document.createDocumentFragment();
        if (channels.length === 0) {
            const message = elements.searchInput.value.trim()
                ? '没有匹配的频道'
                : '暂无频道，请先更新';
            fragment.appendChild(createStateRow(message, 'empty'));
            elements.channelList.replaceChildren(fragment);
            return;
        }

        const groups = new Map();
        channels.forEach(channel => {
            if (!groups.has(channel.group)) groups.set(channel.group, []);
            groups.get(channel.group).push(channel);
        });

        [...groups.keys()].sort((a, b) => a.localeCompare(b, 'zh-CN')).forEach(group => {
            const title = document.createElement('li');
            title.className = 'group-title';
            title.textContent = group;
            title.setAttribute('aria-hidden', 'true');
            fragment.appendChild(title);

            groups.get(group)
                .sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'))
                .forEach(channel => fragment.appendChild(createChannelRow(channel)));
        });
        elements.channelList.replaceChildren(fragment);
    }

    function createChannelRow(channel) {
        const row = document.createElement('li');
        row.className = 'channel-row';
        row.setAttribute('role', 'option');
        row.dataset.channelId = channel.id;
        row.setAttribute('aria-selected', String(channel.id === state.selectedId));

        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'channel-button';
        button.dataset.channelId = channel.id;
        button.setAttribute('aria-selected', String(channel.id === state.selectedId));
        button.setAttribute('aria-label', `播放 ${channel.name}`);

        const placeholder = document.createElement('span');
        placeholder.className = 'channel-logo-placeholder';
        placeholder.textContent = 'TV';

        if (isSafeImageURL(channel.logo)) {
            const image = document.createElement('img');
            image.className = 'channel-logo';
            image.alt = `${channel.name} 台标`;
            image.loading = 'lazy';
            image.decoding = 'async';
            image.src = channel.logo;
            image.addEventListener('error', () => {
                image.hidden = true;
                placeholder.hidden = false;
            }, { once: true });
            placeholder.hidden = true;
            button.appendChild(image);
        }
        button.appendChild(placeholder);

        const details = document.createElement('span');
        details.className = 'channel-details';
        const name = document.createElement('span');
        name.className = 'channel-name';
        name.textContent = channel.name;
        const group = document.createElement('span');
        group.className = 'channel-group';
        group.textContent = channel.group;
        details.append(name, group);
        button.appendChild(details);
        row.appendChild(button);
        return row;
    }

    function onChannelListClick(event) {
        const button = event.target.closest('button[data-channel-id]');
        if (!button) return;
        const channel = state.channels.find(item => item.id === button.dataset.channelId);
        if (channel) playChannel(channel);
    }

    function onSearchInput() {
        window.clearTimeout(state.searchTimer);
        state.searchTimer = window.setTimeout(() => {
            applyFilter(elements.searchInput.value);
        }, 120);
    }

    function applyFilter(rawQuery) {
        const query = rawQuery.trim().toLocaleLowerCase('zh-CN');
        state.filteredChannels = query
            ? state.channels.filter(channel =>
                channel.name.toLocaleLowerCase('zh-CN').includes(query) ||
                channel.group.toLocaleLowerCase('zh-CN').includes(query))
            : [...state.channels];
        renderChannels(state.filteredChannels);
        updateCounts(state.filteredChannels.length);
    }

    function updateCounts(visibleCount) {
        elements.channelCount.textContent = `${state.channels.length} 个频道`;
        elements.visibleCount.textContent = String(visibleCount);
    }

    const streamTypeCache = new Map();

    async function resolveStreamType(channel) {
        if (channel.streamType && channel.streamType !== 'auto') return channel.streamType;
        if (streamTypeCache.has(channel.id)) return streamTypeCache.get(channel.id);
        const info = await fetchJSON(channel.typeURL || `/api/stream/${encodeURIComponent(channel.id)}/type`);
        const kind = typeof info?.stream_type === 'string' && info.stream_type !== 'auto'
            ? info.stream_type
            : 'ts';
        streamTypeCache.set(channel.id, kind);
        if (info?.stream_url) channel.streamURL = info.stream_url;
        return kind;
    }

    function resetRetryCount() {
        state.retryCount = 0;
    }

    function resetPlaybackState() {
        window.clearTimeout(state.stallTimer);
        state.stallTimer = null;
        state.liveWatcher = null;
        state.connection = null;
        state.segmentAlerts = 0;
        state.lastProgressTime = 0;
        state.pausedAt = 0;
        state.autoPaused = false;
        state.loadingSuspended = false;
        state.failureNotified = false;
    }

    async function playChannel(channel, isRetry) {
        if (!isRetry) {
            resetRetryCount();
            state.showFailureStatus = false;
        }
        state.selectedId = channel.id;
        renderChannels(state.filteredChannels);
        elements.channelName.textContent = channel.name;
        elements.playerEmpty.hidden = true;
        elements.channelInfo.hidden = false;
        setChannelStatusInfo('正在连接播放源');
        document.querySelector('.live-indicator').classList.remove('is-live');
        closeMobileDrawer();

        const token = ++state.playbackToken;
        cleanupPlayer();
        resetPlaybackState();

        let streamType = channel.streamType;
        if (streamType === 'auto') {
            try {
                streamType = await resolveStreamType(channel);
            } catch (error) {
                console.warn('[player] stream type probe failed', error);
                streamType = 'ts';
            }
        }
        if (token !== state.playbackToken) return;

        channel.streamType = streamType;
        state.connection = { channel, streamType };
        if (streamType === 'hls') {
            playHLS(channel, token);
        } else {
            playRawTS(channel, token);
        }
    }

    function playHLS(channel, token) {
        if (!channel.playlistURL) {
            showPlaybackError('该频道没有可用的播放地址');
            return;
        }
        const video = elements.player;

        if (window.Hls && window.Hls.isSupported()) {
            const hls = new window.Hls({
                enableWorker: true,
                lowLatencyMode: false,
                backBufferLength: 60,
                manifestLoadingMaxRetry: 3,
                levelLoadingMaxRetry: 3,
                fragLoadingMaxRetry: 3,
                liveMaxLatencyDurationCount: LIVE.hlsMaxLatencyFragments,
                liveSyncDurationCount: LIVE.hlsSyncFragments,
            });
            state.hls = hls;
            let networkRecoveries = 0;
            let mediaRecoveries = 0;
            let notifying = false;

            hls.on(window.Hls.Events.MANIFEST_PARSED, () => {
                if (token !== state.playbackToken) return;
                markLive('播放器已连接 · HLS.js');
                video.play().catch(() => {});
            });
            hls.on(window.Hls.Events.ERROR, (_event, data) => {
                if (token !== state.playbackToken) return;
                if (!data.fatal) {
                    reportSegmentError(data);
                    return;
                }
                console.warn('[player] HLS fatal error', {
                    type: data.type,
                    details: data.details,
                    reason: data.reason,
                });
                if (data.type === window.Hls.ErrorTypes.NETWORK_ERROR && networkRecoveries < 2) {
                    networkRecoveries += 1;
                    setChannelStatusInfo(`正在重试连接（${networkRecoveries}/2）`);
                    hls.startLoad();
                } else if (data.type === window.Hls.ErrorTypes.MEDIA_ERROR && mediaRecoveries < 1) {
                    mediaRecoveries += 1;
                    setChannelStatusInfo('正在恢复媒体');
                    hls.recoverMediaError();
                } else if (!notifying) {
                    notifying = true;
                    notifyPlaybackFailure('HLS', '网络中断');
                }
            });

            hls.loadSource(channel.playlistURL);
            hls.attachMedia(video);
            startLiveWatcher();
            return;
        }

        if (video.canPlayType('application/vnd.apple.mpegurl')) {
            const onLoaded = () => {
                if (token !== state.playbackToken) return;
                markLive('播放器已连接 · 原生 HLS');
                video.play().catch(() => {});
                startLiveWatcher();
            };
            const onError = () => {
                if (token === state.playbackToken) notifyPlaybackFailure('HLS', '无法继续播放');
            };
            state.videoLoadedHandler = onLoaded;
            state.videoErrorHandler = onError;
            video.addEventListener('loadedmetadata', onLoaded, { once: true });
            video.addEventListener('error', onError, { once: true });
            video.src = channel.playlistURL;
            video.load();
            return;
        }

        showPlaybackError('当前浏览器不支持 HLS 播放');
    }

    function playRawTS(channel, token) {
        if (!channel.streamURL) {
            showPlaybackError('该频道没有可用的播放地址');
            return;
        }
        if (!window.mpegts || !window.mpegts.isSupported()) {
            showPlaybackError('当前浏览器不支持 MPEG-TS 播放');
            return;
        }
        const video = elements.player;
        const mpegtsPlayer = window.mpegts.createPlayer({
            type: 'mpegts',
            isLive: true,
            url: channel.streamURL,
        }, {
            enableStashBuffer: true,
            stashInitialSize: 1024 * 1024,
            lazyLoad: false,
            isLive: true,
            liveBufferLatencyChasing: true,
            liveBufferLatencyMaxLatency: 3.0,
            liveBufferLatencyMinRemain: 1.0,
            liveSync: false,
            fixAudioTimestampGap: true,
            autoCleanupSourceBuffer: true,
            autoCleanupMaxBackwardDuration: 60,
            autoCleanupMinBackwardDuration: 30,
            accurateSeek: false,
        });
        state.mpegts = mpegtsPlayer;
        mpegtsPlayer.attachMediaElement(video);

        let started = false;
        let notifying = false;

        const startPlayback = () => {
            if (started || token !== state.playbackToken) return;
            started = true;
            markLive('播放器已连接 · MPEG-TS');
            try {
                const result = mpegtsPlayer.play();
                if (result && typeof result.catch === 'function') {
                    result.catch(() => {
                        if (token === state.playbackToken) showPlaybackError('浏览器拒绝播放，请手动点击播放');
                    });
                }
                startLiveWatcher();
            } catch (_error) {
                if (token === state.playbackToken) showPlaybackError('MPEG-TS 播放失败：无法启动媒体');
            }
        };

        mpegtsPlayer.on(window.mpegts.Events.ERROR, (errorType, errorDetail, errorInfo) => {
            console.warn('[player] MPEG-TS error', { errorType, errorDetail, errorInfo, url: channel.streamURL });
            if (token !== state.playbackToken) return;
            if (errorType === 'NetworkError' && !notifying) {
                notifying = true;
                notifyPlaybackFailure('MPEG-TS', errorDetail || '无法继续播放');
                return;
            }
            showPlaybackError(`MPEG-TS 播放失败：${errorDetail || errorType || '上游流不可用'}`);
        });
        mpegtsPlayer.on(window.mpegts.Events.MEDIA_INFO, startPlayback);

        const onLoaded = () => {
            startPlayback();
            state.lastProgressTime = video.currentTime;
        };
        const onError = () => {
            const mediaError = video.error;
            console.warn('[player] media element error', mediaError);
            if (token !== state.playbackToken) return;
            showPlaybackError(mediaError && mediaError.code === 4
                ? 'MPEG-TS 播放失败：浏览器无法解码该流（可能是音频编码不受支持）'
                : `MPEG-TS 播放失败：媒体错误 ${mediaError?.code || 'unknown'}`);
        };
        state.videoLoadedHandler = onLoaded;
        state.videoErrorHandler = onError;
        video.addEventListener('loadedmetadata', onLoaded, { once: true });
        video.addEventListener('error', onError, { once: true });
        mpegtsPlayer.load();
    }

    function markLive(message) {
        state.pausedAt = 0;
        state.loadingSuspended = false;
        document.querySelector('.live-indicator').classList.add('is-live');
        setChannelStatusInfo(message);
    }

    function mediaBufferEnd(video) {
        const buffered = video.buffered;
        if (!buffered || buffered.length === 0) return null;
        const end = buffered.end(buffered.length - 1);
        return Number.isFinite(end) && end > 0 ? end : null;
    }

    function hlsLiveEdge() {
        if (!state.hls) return null;
        const details = state.hls.latestLevelDetails;
        if (!details || !details.live) return null;
        const edge = details.edge + (details.age || 0);
        return Number.isFinite(edge) ? edge : null;
    }

    function liveEdgeTarget(video, force) {
        const hlsEdge = hlsLiveEdge();
        const current = video.currentTime;
        if (!Number.isFinite(current)) return null;

        if (hlsEdge !== null) {
            if (!force && hlsEdge - current <= LIVE.maxLatency) return null;
            return Math.max(0, hlsEdge - LIVE.targetLatency);
        }

        const bufferEnd = mediaBufferEnd(video);
        if (bufferEnd === null) return null;
        if (!force && bufferEnd - current <= LIVE.maxLatency) return null;
        const target = Math.max(0, bufferEnd - LIVE.targetLatency);
        return target > current ? target : null;
    }

    function seekToLiveEdge(reason, force) {
        if (state.showFailureStatus) return;
        const video = elements.player;
        if (!video || video.readyState === 0 || video.seeking) return;

        const target = liveEdgeTarget(video, force);
        if (target === null) return;

        const from = video.currentTime;
        if (target <= from) return;
        try {
            video.currentTime = target;
        } catch (_error) {
            return;
        }
        console.log('[player] 追回直播边缘', { reason, from, to: target, latency: target - from });

        if (state.hls) {
            try {
                state.hls.startLoad(target);
            } catch (_error) {}
        }
        state.lastProgressTime = video.currentTime;
        if (state.connection) {
            setChannelStatusInfo(`已回到直播最新位置 · ${state.connection.streamType === 'hls' ? 'HLS' : 'MPEG-TS'}`);
        }
    }

    function startLiveWatcher() {
        stopLiveWatcher();
        if (state.connection && state.connection.streamType === 'hls') return;
        const interval = window.setInterval(() => {
            const video = elements.player;
            if (video && !video.paused && !video.seeking) seekToLiveEdge('watchdog');
        }, 2000);
        state.liveWatcher = () => window.clearInterval(interval);
    }

    function stopLiveWatcher() {
        if (state.liveWatcher) {
            state.liveWatcher();
            state.liveWatcher = null;
        }
    }

    function mpegtsTransmuxer() {
        const engine = state.mpegts && state.mpegts._player_engine;
        return engine && engine._transmuxer ? engine._transmuxer : null;
    }

    function suspendLoading() {
        if (state.loadingSuspended || state.showFailureStatus) return;
        state.loadingSuspended = true;

        if (state.hls) {
            try {
                state.hls.stopLoad();
            } catch (_error) {}
        }
        const transmuxer = mpegtsTransmuxer();
        if (transmuxer) {
            try {
                transmuxer.pause();
            } catch (_error) {}
        }
    }

    function resumeLoading() {
        if (!state.loadingSuspended) return;
        state.loadingSuspended = false;

        if (state.hls) {
            const video = elements.player;
            const current = video && Number.isFinite(video.currentTime) ? video.currentTime : -1;
            try {
                state.hls.startLoad(current);
            } catch (_error) {}
        }
    }

    function resumeMpegts() {
        const transmuxer = mpegtsTransmuxer();
        if (!transmuxer) return;
        try {
            transmuxer.resume();
        } catch (_error) {}
    }

    function watchPlaybackProgress() {
        state.stallTimer = null;
        const video = elements.player;
        const connection = state.connection;
        if (!video || !connection) return;
        if (video.seeking || video.readyState === 0 || video.paused) return;
        const current = video.currentTime;
        if (current > state.lastProgressTime + LIVE.stallProgress) return;

        console.warn('[player] 恢复播放后进度停滞，重新连接', {
            currentTime: current,
            lastProgressTime: state.lastProgressTime,
            readyState: video.readyState,
        });
        reconnectPlayback('播放已停滞，正在重新连接最新流');
    }

    function reconnectPlayback(reason, userInitiated) {
        if (state.showFailureStatus) return;

        const connection = state.connection;
        if (!connection) return;

        if (!userInitiated && state.retryCount >= LIVE.maxRetries) {
            state.showFailureStatus = true;
            state.retryCount = LIVE.maxRetries;
            state.playbackToken += 1;
            cleanupPlayer();
            document.querySelector('.live-indicator').classList.remove('is-live');
            showToast(`已重试 ${LIVE.maxRetries} 次仍未恢复，请稍后重新选择频道`, 'error');
            setChannelStatusInfo('播放失败，请重新选择频道');
            return;
        }

        if (!userInitiated) state.retryCount += 1;

        const retryToken = state.playbackToken;
        if (reason) showToast(reason, 'warning');
        else if (!userInitiated && state.retryCount > 1) showToast(`正在重连（${state.retryCount}/${LIVE.maxRetries}）`, 'warning');
        window.setTimeout(() => {
            if (retryToken !== state.playbackToken) return;
            playChannel(connection.channel, true);
        }, 0);
    }

    function notifyPlaybackFailure(kind, detail) {
        if (state.failureNotified || state.showFailureStatus) return;
        state.failureNotified = true;
        showPlaybackError(`${kind} 播放中断（${detail}），正在重新连接最新流`);
        reconnectPlayback('');
    }

    function reportSegmentError(data) {
        const details = data.details || '';
        if (!/LoadError|TimeOut|TimeOutError/i.test(details)) return;
        if (state.segmentAlerts > 0) return;
        state.segmentAlerts += 1;
        setChannelStatusInfo('直播源暂时中断，正在自动重试');
    }

    function setChannelStatusInfo(message) {
        elements.channelInfoText.textContent = message;
    }

    function cleanupPlayer() {
        stopLiveWatcher();
        if (state.mpegts) {
            state.mpegts.destroy();
            state.mpegts = null;
        }
        if (state.hls) {
            state.hls.destroy();
            state.hls = null;
        }
        const video = elements.player;
        if (state.videoLoadedHandler) {
            video.removeEventListener('loadedmetadata', state.videoLoadedHandler);
            state.videoLoadedHandler = null;
        }
        if (state.videoErrorHandler) {
            video.removeEventListener('error', state.videoErrorHandler);
            state.videoErrorHandler = null;
        }
        state.tearingDown = true;
        video.pause();
        video.removeAttribute('src');
        video.load();
        window.setTimeout(() => {
            state.tearingDown = false;
        }, 0);
    }

    function stopPlayback() {
        state.playbackToken += 1;
        cleanupPlayer();
        elements.playerEmpty.hidden = false;
        elements.channelInfo.hidden = true;
        elements.channelName.textContent = '未选择频道';
        document.querySelector('.live-indicator').classList.remove('is-live');
    }

    function showPlaybackError(message) {
        elements.channelInfo.hidden = false;
        setChannelStatusInfo(message);
        document.querySelector('.live-indicator').classList.remove('is-live');
        showToast(message, 'error');
    }

    async function updateChannels() {
        if (state.updateInFlight) return;
        state.updateInFlight = true;
        setUpdateButtonLoading(true);
        setUpdateStatus('正在更新频道', 'warning');
        try {
            await fetchJSON('/api/update', { method: 'POST' });
            showToast('频道列表更新成功', 'success');
            await pollStatus();
            if (!state.catalogReady) await loadChannels();
        } catch (_error) {
            setUpdateStatus('更新失败', 'error');
            showToast('频道更新失败，请稍后重试', 'error');
        } finally {
            state.updateInFlight = false;
            setUpdateButtonLoading(false);
            if (state.catalogReady) setUpdateStatus(formatCatalogStatus(state.channels.length), 'normal');
        }
    }

    async function pollStatus() {
        if (state.statusInFlight) return;
        state.statusInFlight = true;
        try {
            const data = await fetchJSON('/api/status');
            setConnectionStatus('在线', true);
            if (data.last_update) {
                const date = new Date(data.last_update);
                const time = Number.isNaN(date.getTime()) ? '时间未知' : date.toLocaleString('zh-CN');
                setUpdateStatus(`${data.channel_count || 0} 个频道 · ${time}`, 'normal');
            }

            const nextHash = typeof data.hash === 'string' ? data.hash : '';
            if (!state.hashInitialized) {
                state.currentHash = nextHash;
                state.pendingHash = nextHash;
                state.hashInitialized = true;
            } else if (nextHash !== state.currentHash) {
                state.currentHash = nextHash;
                await loadChannels();
            }
        } catch (_error) {
            setConnectionStatus('离线', false);
        } finally {
            state.statusInFlight = false;
        }
    }

    function setListState(message, kind) {
        const row = createStateRow(message, kind);
        elements.channelList.replaceChildren(row);
    }

    function createStateRow(message, kind) {
        const row = document.createElement('li');
        row.className = `state-row ${kind || ''}`;
        row.textContent = message;
        return row;
    }

    function setUpdateButtonLoading(loading) {
        elements.updateBtn.disabled = loading;
        elements.updateBtn.classList.toggle('is-loading', loading);
        const label = elements.updateBtn.querySelector('span:last-child');
        if (label) label.textContent = loading ? '更新中' : '手动更新';
    }

    function setUpdateStatus(text, kind) {
        elements.updateStatus.textContent = text;
        elements.updateStatus.dataset.state = kind;
    }

    function setConnectionStatus(text, online) {
        elements.connectionStatus.textContent = text;
        elements.connectionStatus.parentElement.classList.toggle('offline', !online);
    }

    function formatCatalogStatus(count) {
        return count > 0 ? `${count} 个频道已就绪` : '暂无频道';
    }

    function showToast(message, kind = '') {
        window.clearTimeout(state.toastTimer);
        elements.toast.textContent = message;
        elements.toast.className = `toast visible ${kind}`;
        state.toastTimer = window.setTimeout(() => {
            elements.toast.className = 'toast';
        }, 3200);
    }

    function isMobileViewport() {
        return window.matchMedia('(max-width: 768px)').matches;
    }

    function toggleSidebar(forceOpen) {
        const mobile = isMobileViewport();
        const isOpen = mobile
            ? elements.sidebar.classList.contains('open')
            : !elements.sidebar.classList.contains('collapsed');
        const open = typeof forceOpen === 'boolean' ? forceOpen : !isOpen;

        if (mobile) {
            elements.sidebar.classList.toggle('open', open);
            elements.sidebar.classList.remove('collapsed');
            elements.overlay.classList.toggle('active', open);
            elements.overlay.setAttribute('aria-hidden', String(!open));
        } else {
            elements.sidebar.classList.toggle('collapsed', !open);
            elements.sidebar.classList.remove('open');
            elements.overlay.classList.remove('active');
            elements.overlay.setAttribute('aria-hidden', 'true');
        }

        elements.menuToggle.setAttribute('aria-expanded', String(open));
        elements.menuToggle.setAttribute('aria-label', open ? '收起频道列表' : '打开频道列表');
        elements.menuToggle.textContent = open ? '收起频道' : '打开频道';
        elements.sidebarClose.setAttribute('aria-label', open ? '收起频道列表' : '打开频道列表');
        elements.sidebarClose.textContent = open ? '收起' : '打开';
        document.body.classList.toggle('drawer-open', mobile && open);
    }

    function closeMobileDrawer() {
        if (isMobileViewport()) toggleSidebar(false);
    }

    function closeSidebar() {
        toggleSidebar(false);
    }

    function onDocumentKeydown(event) {
        if (event.key === 'Escape') closeSidebar();
        if (event.key === '/' && document.activeElement !== elements.searchInput) {
            event.preventDefault();
            elements.searchInput.focus();
        }
    }

    function onPlayerPause() {
        if (state.tearingDown) return;
        state.pausedAt = Date.now();
        suspendLoading();
    }

    function onPlayerPlay() {
        if (!state.connection || state.showFailureStatus) return;
        const streamType = state.connection.streamType;
        const pausedFor = state.pausedAt ? Date.now() - state.pausedAt : 0;
        const video = elements.player;

        state.pausedAt = 0;
        const wasSuspended = state.loadingSuspended;
        const longPause = pausedFor >= LIVE.pausedGrace * 1000;

        if (wasSuspended && streamType === 'ts' && longPause) {
            state.loadingSuspended = false;
            reconnectPlayback('', true);
            return;
        }

        if (longPause) {
            seekToLiveEdge('resume', true);
            if (mediaBufferEnd(video) === null) {
                state.loadingSuspended = false;
                reconnectPlayback('暂停期间缓冲已失效，正在重新连接最新流', true);
                return;
            }
        }

        if (wasSuspended) {
            if (streamType === 'ts') {
                resumeMpegts();
            } else {
                resumeLoading();
            }
        }

        if (!longPause) {
            state.lastProgressTime = video.currentTime;
            return;
        }

        state.lastProgressTime = video.currentTime;
        window.clearTimeout(state.stallTimer);
        state.stallTimer = window.setTimeout(watchPlaybackProgress, LIVE.stallProbeDelay);
    }

    function onVisibilityChange() {
        const video = elements.player;
        if (document.visibilityState === 'hidden') {
            if (state.connection && !video.paused) {
                state.autoPaused = true;
                try {
                    video.pause();
                } catch (_error) {}
            }
            return;
        }
        if (!state.connection) return;

        if (video.paused) {
            if (!state.autoPaused) return;
            state.autoPaused = false;
            settleLiveEdge();
            return;
        }

        seekToLiveEdge('visibility', true);
        state.lastProgressTime = video.currentTime;
    }

    function settleLiveEdge() {
        const video = elements.player;
        if (!video || state.showFailureStatus) return;
        seekToLiveEdge('resume', true);
        const result = video.play();
        if (result && typeof result.catch === 'function') result.catch(() => {});
        state.lastProgressTime = video.currentTime;
        window.clearTimeout(state.stallTimer);
        state.stallTimer = window.setTimeout(watchPlaybackProgress, LIVE.stallProbeDelay);
    }

    function isSafeImageURL(raw) {
        if (!raw) return false;
        try {
            const parsed = new URL(raw, window.location.origin);
            return parsed.protocol === 'http:' || parsed.protocol === 'https:';
        } catch (_error) {
            return false;
        }
    }
})();
