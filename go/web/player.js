/* Native media playback. Local progress and subtitles never upload to WPS. */
(() => {
  "use strict";
  const AUDIO = /\.(mp3|m4a|aac|ogg|oga|wav|flac)$/i;
  const VIDEO = /\.(mp4|m4v|webm|ogv)$/i;
  const MAX_RECORDS = 200;
  const MAX_STORED_BYTES = 160 * 1024;
  const MAX_SUBTITLE_BYTES = 1024 * 1024;
  function node(tag, className, text) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined) item.textContent = text;
    return item;
  }
  function button(text, id, action) {
    const item = node("button", "", text);
    item.type = "button";
    item.id = id;
    item.addEventListener("click", action);
    return item;
  }
  function clock(seconds) {
    const value = Math.floor(seconds);
    return `${Math.floor(value / 60)}:${String(value % 60).padStart(2, "0")}`;
  }
  window.WPSMediaPlayer = {
    supports(name) { return typeof name === "string" && (AUDIO.test(name) || VIDEO.test(name)); },
    init(config) {
      const container = document.getElementById("preview-media");
      const loading = document.getElementById("preview-loading");
      const errorNode = document.getElementById("preview-error");
      let generation = 0, subtitleGeneration = 0, current = null;
      let subtitleURL = null, subtitleTrack = null;
      let progressNote = null, subtitleNote = null, subtitleClear = null;
      let ownerKey = null, progressKey = null, preferenceKey = null;
      let records = [], speed = 1, lastSavedAt = 0;
      let playlist = [], index = -1;

      function readUser() {
        const username = String(config.username() || "local");
        const nextKey = encodeURIComponent(username);
        if (nextKey === ownerKey) return;
        ownerKey = nextKey;
        progressKey = "wpsdrv.media-progress." + nextKey;
        preferenceKey = "wpsdrv.media-speed." + nextKey;
        records = [];
        speed = 1;
        try {
          const raw = localStorage.getItem(progressKey);
          if (raw && raw.length <= MAX_STORED_BYTES) {
            const parsed = JSON.parse(raw);
            if (Array.isArray(parsed)) records = parsed.filter((item) => item && typeof item.path === "string" && item.path.length <= 4096 && typeof item.identity === "string" && item.identity.length <= 8192 && Number.isFinite(item.position) && item.position >= 0 && Number.isFinite(item.updated)).slice(-MAX_RECORDS);
          }
          const saved = Number(localStorage.getItem(preferenceKey));
          if ([0.5, 0.75, 1, 1.25, 1.5, 1.75, 2].includes(saved)) speed = saved;
        } catch (_) { /* Browser privacy settings may disable local storage. */ }
      }
      function identity(entry) { return JSON.stringify([entry.id || "", entry.size || 0, entry.modified_at || 0, entry.etag || ""]); }
      function saveProgress(force = false) {
        if (!current || !current.ready || (!force && Date.now() - lastSavedAt < 5000)) return;
        const media = current.media;
        const position = media.currentTime;
        if (!Number.isFinite(position) || !Number.isFinite(media.duration) || media.duration <= 0) return;
        records = records.filter((item) => item.path !== current.path);
        if (current.path.length <= 4096 && position >= 1 && !media.ended && media.duration - position > 3) records.push({ path: current.path, identity: identity(current.entry), position, updated: Date.now() });
        records = records.slice(-MAX_RECORDS);
        try {
          let encoded = JSON.stringify(records);
          while (encoded.length > MAX_STORED_BYTES && records.length) { records.shift(); encoded = JSON.stringify(records); }
          localStorage.setItem(progressKey, encoded);
        } catch (_) { /* Playback remains usable without saved progress. */ }
        lastSavedAt = Date.now();
      }
      function clearSubtitle() {
        subtitleGeneration += 1;
        if (subtitleTrack) subtitleTrack.remove();
        subtitleTrack = null;
        if (subtitleURL) URL.revokeObjectURL(subtitleURL);
        subtitleURL = null;
        if (subtitleNote) subtitleNote.textContent = "字幕仅在本页使用，不会上传。";
        if (subtitleClear) subtitleClear.disabled = true;
      }
      function clear() {
        saveProgress(true);
        generation += 1;
        clearSubtitle();
        if (current) {
          const media = current.media;
          // Remove listeners before load(): releasing a source may emit more
          // error/ended events, which must not change the next preview.
          for (const [name, callback] of current.listeners) media.removeEventListener(name, callback);
          media.pause();
          media.removeAttribute("src");
          media.load();
          media.remove();
        }
        current = null;
        playlist = [];
        index = -1;
        progressNote = subtitleNote = subtitleClear = null;
        container.classList.remove("has-player");
      }
      function changeTrack(offset, shouldPlay = null) {
        if (!current) return;
        const next = playlist[index + offset];
        if (!next) return;
        const play = shouldPlay === null ? !current.media.paused : shouldPlay;
        config.open(next.entry, next.path, play);
      }
      function attemptPlay(record) {
        const attempt = record.media.play();
        if (attempt && typeof attempt.catch === "function") attempt.catch(() => {
          if (current === record && record.generation === generation && progressNote) progressNote.textContent = "点击播放器中的播放按钮开始播放。";
        });
      }
      async function chooseSubtitle(file) {
        if (!file || !current) return;
        const record = current;
        const ticket = ++subtitleGeneration;
        try {
          if (!/\.(vtt|srt)$/i.test(file.name)) throw new Error("请选择 WebVTT（.vtt）或 SRT 字幕文件。");
          if (file.size > MAX_SUBTITLE_BYTES) throw new Error("字幕文件不能超过 1 MiB。");
          let text;
          try { text = new TextDecoder("utf-8", { fatal: true }).decode(await file.arrayBuffer()); }
          catch (_) { throw new Error("字幕必须使用 UTF-8 编码。"); }
          if (/\.srt$/i.test(file.name)) {
            const cues = text.replace(/\r\n?/g, "\n").trim().split(/\n[ \t]*\n/);
            for (const cue of cues) {
              const lines = cue.split("\n");
              const timing = /^\d+$/.test(lines[0].trim()) ? lines[1] : lines[0];
              if (!timing || !/^\d{2,}:\d{2}:\d{2}[,.]\d{3}\s+-->\s+\d{2,}:\d{2}:\d{2}[,.]\d{3}\s*$/.test(timing)) throw new Error("SRT 字幕时间轴无效，请选择标准 UTF-8 字幕。");
            }
            text = "WEBVTT\n\n" + text.replace(/(\d{2,}:\d{2}:\d{2}),(\d{3})/g, "$1.$2");
          }
          if (!/^WEBVTT(?:[ \t][^\r\n]*)?(?:\r?\n|$)/.test(text) || text.includes("\u0000")) throw new Error("字幕必须是有效的 UTF-8 WebVTT 文件。");
          if (current !== record || ticket !== subtitleGeneration) return;
          clearSubtitle();
          const track = node("track");
          subtitleTrack = track;
          track.kind = "subtitles";
          track.label = file.name;
          track.srclang = "und";
          track.default = true;
          subtitleURL = URL.createObjectURL(new Blob([text], { type: "text/vtt;charset=utf-8" }));
          track.src = subtitleURL;
          track.addEventListener("load", () => {
            if (current === record && subtitleTrack === track) track.track.mode = "showing";
          });
          track.addEventListener("error", () => {
            if (current === record && subtitleTrack === track) subtitleNote.textContent = "字幕无法解析，请检查时间轴和文件格式。";
          });
          record.media.append(track);
          track.track.mode = "showing";
          subtitleNote.textContent = `本地字幕：${file.name}`;
          subtitleClear.disabled = false;
        } catch (error) {
          if (current === record && ticket === subtitleGeneration) subtitleNote.textContent = error.message || "无法读取字幕文件。";
        }
      }
      function open(entry, path, items, autoplay = false) {
        clear();
        readUser();
        const epoch = generation;
        const video = VIDEO.test(entry.name);
        playlist = items.filter((item) => window.WPSMediaPlayer.supports(item.entry.name));
        if (!playlist.some((item) => item.path === path)) playlist.unshift({ entry, path });
        index = playlist.findIndex((item) => item.path === path);
        const media = node(video ? "video" : "audio", "native-player");
        media.id = "preview-player";
        media.controls = true;
        media.preload = "metadata";
        media.setAttribute("aria-label", entry.name);
        if (video) media.playsInline = true;
        const record = { entry, path, media, generation: epoch, ready: false, hasPlayed: false, advancing: false, listeners: [] };
        current = record;
        const listen = (name, callback) => {
          const guarded = (...args) => { if (current === record && epoch === generation) callback(...args); };
          record.listeners.push([name, guarded]);
          media.addEventListener(name, guarded);
        };
        const tools = node("div", "player-toolbar");
        const previous = button("上一首 / 集", "player-previous", () => changeTrack(-1));
        const next = button("下一首 / 集", "player-next", () => changeTrack(1));
        previous.disabled = index <= 0;
        next.disabled = index >= playlist.length - 1;
        const speedLabel = node("label", "", "速度 ");
        const rate = node("select");
        rate.id = "player-rate";
        for (const value of [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2]) {
          const option = node("option", "", `${value}×`);
          option.value = String(value);
          rate.append(option);
        }
        rate.value = String(speed);
        rate.addEventListener("change", () => {
          if (current !== record) return;
          speed = Number(rate.value);
          media.playbackRate = speed;
          try { localStorage.setItem(preferenceKey, String(speed)); } catch (_) {}
        });
        speedLabel.append(rate);
        tools.append(previous, next, speedLabel);
        const choiceLabel = node("label", "player-playlist", `播放列表（${index + 1}/${playlist.length}）`);
        const choice = node("select");
        choice.id = "player-playlist";
        playlist.forEach((item, itemIndex) => {
          const option = node("option", "", item.entry.name);
          option.value = String(itemIndex);
          choice.append(option);
        });
        choice.value = String(index);
        choice.addEventListener("change", () => {
          if (current !== record) return;
          const next = playlist[Number(choice.value)];
          if (next) config.open(next.entry, next.path, !media.paused);
        });
        choiceLabel.append(choice);
        progressNote = node("p", "preview-meta player-progress-note", "播放位置会保存在当前浏览器。首次打开不会自动播放。");
        progressNote.id = "player-progress-note";
        const restart = button("从头开始", "player-restart", () => {
          if (current !== record) return;
          media.currentTime = 0;
          saveProgress(true);
          progressNote.textContent = "已回到开头。";
        });
        restart.disabled = true;
        tools.append(restart);
        container.classList.add("has-player");
        container.hidden = false;
        container.replaceChildren(media, tools, choiceLabel, progressNote);
        if (video) {
          const subtitles = node("div", "player-subtitles");
          const picker = node("input");
          picker.type = "file";
          picker.id = "player-subtitle-input";
          picker.accept = ".vtt,.srt,text/vtt,application/x-subrip";
          picker.hidden = true;
          picker.addEventListener("change", () => { chooseSubtitle(picker.files && picker.files[0]); picker.value = ""; });
          const pick = button("选择本地字幕", "player-subtitle-pick", () => picker.click());
          subtitleClear = button("移除字幕", "player-subtitle-clear", clearSubtitle);
          subtitleClear.disabled = true;
          subtitleNote = node("p", "preview-meta player-subtitle-note", "字幕仅在本页使用，不会上传。");
          subtitleNote.id = "player-subtitle-note";
          subtitles.append(pick, subtitleClear, picker, subtitleNote);
          container.append(subtitles);
        }
        listen("loadedmetadata", () => {
          loading.hidden = true;
          record.ready = true;
          media.playbackRate = speed;
          restart.disabled = !Number.isFinite(media.duration);
          const saved = records.find((item) => item.path === path && item.identity === identity(entry));
          if (saved && saved.position >= 1 && Number.isFinite(media.duration) && saved.position < media.duration - 3) {
            try { media.currentTime = saved.position; progressNote.textContent = `已恢复到 ${clock(saved.position)}，点击播放继续。`; } catch (_) {}
          }
          if (autoplay) attemptPlay(record);
        });
        listen("play", () => { record.hasPlayed = true; });
        listen("timeupdate", () => saveProgress());
        listen("pause", () => saveProgress(true));
        listen("ended", () => {
          saveProgress(true);
          if (record.hasPlayed && !record.advancing && index < playlist.length - 1) {
            record.advancing = true;
            changeTrack(1, true);
          }
        });
        listen("error", () => {
          loading.hidden = true;
          restart.disabled = true;
          const code = media.error && media.error.code;
          errorNode.textContent = code === 2 ? "读取媒体失败，请检查连接或重新登录，也可以下载后播放。" : "浏览器无法播放此文件，文件可能损坏或使用了不支持的编码。请下载后用本地播放器打开。";
        });
        media.src = config.source(path);
        media.load();
      }
      document.addEventListener("visibilitychange", () => { if (document.hidden) saveProgress(true); });
      window.addEventListener("pagehide", () => saveProgress(true));
      return { open, clear };
    },
  };
})();
