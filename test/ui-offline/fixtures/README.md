# Offline media fixtures

`native-video.webm` is a repository-authored synthetic solid-color clip. It contains no personal media, external footage, audio, or credentials. The browser regression reads this checked-in fixture directly; running the tests does **not** require FFmpeg.

The clip was generated with:

```sh
ffmpeg -v error -f lavfi -i 'color=c=0x168bd2:s=320x180:r=10:d=12' \
  -c:v libvpx -b:v 100k -an -f webm native-video.webm
```

Its fixed playback properties are VP8 in a WebM container, 320 × 180 pixels, 10 frames per second, a 12-second duration, a solid `#168bd2` background, and no audio track. These let `media_player.py` check native decoding, seeking, end-of-playlist behavior, and local subtitle rendering without accessing WPS or another service. Regeneration may change container metadata and encoded bytes across FFmpeg/libvpx versions; the regression depends on the playback properties, not a byte-identical re-encode.

The WAV audio used by the same test is generated in memory by Python's standard library: 12 seconds of a 220 Hz tone, mono, signed 16-bit PCM, 8 kHz sample rate. It is not written to the repository.
