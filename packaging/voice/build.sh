#!/bin/sh
# Builds the voice bundle a release carries for this machine:
#   dist/mirrin-voice-<os>-<arch>.tar.gz
# holding mirrin-voice/whisper-server (whisper.cpp) and mirrin-voice/sox
# (SoX, with MP3 decoding for ElevenLabs), plus their licences and where
# their source is. One-click voice setup downloads it, checked against the
# release's SHA256SUMS, so voice works without Homebrew or a package manager.
#
# Run it on the platform it's for (the release workflow runs it on each
# kind of machine): `make voice-bundle`. It needs curl, cmake, make and a
# C/C++ compiler, and on Linux libasound2-dev for the microphone.
#
# The sources are pinned by checksum. Everything is linked statically except
# the system's own libraries (and ALSA's libasound on Linux, which every
# desktop has), so the programs run from wherever the bundle is unpacked.
set -eu

WHISPER_VERSION=1.9.4
WHISPER_URL="https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v$WHISPER_VERSION.tar.gz"
WHISPER_SHA256=57e280cee375ab02425b806ad5146b99f6eb9357e3c2b31357c8a6af2e2e44ae
SOX_VERSION=14.4.2
SOX_URL="https://downloads.sourceforge.net/project/sox/sox/$SOX_VERSION/sox-$SOX_VERSION.tar.bz2"
SOX_SHA256=81a6956d4330e75b5827316e44ae381e6f1e8928003c6aa45896da9041ea149c
# libmad, as maintained by the Tenacity project (it builds with today's
# compilers), for SoX's MP3 decoding.
MAD_VERSION=0.16.4
MAD_URL="https://codeberg.org/tenacityteam/libmad/archive/$MAD_VERSION.tar.gz"
MAD_SHA256=f4eb229452252600ce48f3c2704c9e6d97b789f81e31c37b0c67dd66f445ea35

say() { printf '%s\n' "$*"; }
fail() {
  printf 'voice bundle: %s\n' "$*" >&2
  exit 1
}

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux) OS=linux ;;
  *) fail "builds on macOS and Linux only" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  arm64 | aarch64) ARCH=arm64 ;;
  *) fail "no voice bundle for $(uname -m)" ;;
esac
OUT=${DIST:-dist}
NAME="mirrin-voice-$OS-$ARCH"
JOBS=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# fetch URL FILE SHA256 downloads a source and refuses it unless it matches.
fetch() {
  curl -fsSL --retry 3 -o "$2" "$1" || fail "couldn't download $1"
  got=$(sha256 "$2")
  [ "$got" = "$3" ] || fail "$1 doesn't match its pinned checksum (got $got)"
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
STAGE="$WORK/mirrin-voice"
mkdir -p "$STAGE" "$OUT"

say "building whisper-server $WHISPER_VERSION for $OS/$ARCH"
fetch "$WHISPER_URL" "$WORK/whisper.tar.gz" "$WHISPER_SHA256"
tar -xzf "$WORK/whisper.tar.gz" -C "$WORK"
WSRC="$WORK/whisper.cpp-$WHISPER_VERSION"
# No -march=native: the build machine's CPU isn't the user's. amd64 assumes
# AVX2 (Intel since 2013, AMD since 2015); Apple silicon and arm64 Linux use
# their baseline. On macOS the Metal shaders are embedded in the program.
set -- -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF \
  -DWHISPER_BUILD_EXAMPLES=ON -DWHISPER_BUILD_SERVER=ON -DWHISPER_BUILD_TESTS=OFF -DWHISPER_CURL=OFF -DWHISPER_SDL2=OFF
if [ "$ARCH" = amd64 ]; then
  set -- "$@" -DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON
fi
if [ "$OS" = darwin ]; then
  set -- "$@" -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON -DCMAKE_OSX_DEPLOYMENT_TARGET=12.0
else
  set -- "$@" "-DCMAKE_EXE_LINKER_FLAGS=-static-libstdc++ -static-libgcc"
fi
cmake -S "$WSRC" -B "$WORK/whisper-build" "$@" >/dev/null
cmake --build "$WORK/whisper-build" --target whisper-server -j "$JOBS" >/dev/null
cp "$WORK/whisper-build/bin/whisper-server" "$STAGE/whisper-server"
cp "$WSRC/LICENSE" "$STAGE/LICENSE.whisper.cpp.txt"

say "building libmad $MAD_VERSION (MP3) for $OS/$ARCH"
fetch "$MAD_URL" "$WORK/libmad.tar.gz" "$MAD_SHA256"
tar -xzf "$WORK/libmad.tar.gz" -C "$WORK"
MAD="$WORK/mad"
cmake -S "$WORK/libmad" -B "$WORK/mad-build" -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DEXAMPLE=OFF \
  -DCMAKE_INSTALL_PREFIX="$MAD" -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_OSX_DEPLOYMENT_TARGET=12.0 >/dev/null
cmake --build "$WORK/mad-build" -j "$JOBS" >/dev/null
cmake --install "$WORK/mad-build" >/dev/null
if [ ! -f "$MAD/lib/libmad.a" ] || [ ! -f "$MAD/include/mad.h" ]; then fail "libmad didn't build"; fi
cp "$WORK/libmad/COPYING" "$STAGE/COPYING.libmad.txt" 2>/dev/null || true

say "building sox $SOX_VERSION for $OS/$ARCH"
fetch "$SOX_URL" "$WORK/sox.tar.bz2" "$SOX_SHA256"
tar -xjf "$WORK/sox.tar.bz2" -C "$WORK"
SSRC="$WORK/sox-$SOX_VERSION"
if [ "$OS" = darwin ]; then
  audio="--with-coreaudio --without-alsa"
  export MACOSX_DEPLOYMENT_TARGET=12.0
else
  audio="--with-alsa --without-coreaudio"
fi
# Only what voice uses: WAV and raw in and out, MP3 in, and the sound card.
# SoX 14.4.2 predates today's compilers' stricter defaults; these are its
# known warnings, not errors in what voice uses.
# shellcheck disable=SC2086 # $audio is two flags
(cd "$SSRC" && CFLAGS="-O2 -Wno-incompatible-function-pointer-types -Wno-incompatible-pointer-types -Wno-implicit-function-declaration -Wno-int-conversion" \
  CPPFLAGS="-I$MAD/include" LDFLAGS="-L$MAD/lib" ./configure --quiet \
  --disable-shared --enable-static --disable-openmp --without-libltdl $audio \
  --without-pulseaudio --without-ao --without-oss --without-sndio --without-png --without-ladspa \
  --without-magic --without-lame --without-twolame --without-id3tag --without-flac --without-oggvorbis \
  --without-opus --without-sndfile --without-wavpack --without-amrwb --without-amrnb --without-lpc10 \
  --without-gsm >/dev/null)
make -C "$SSRC" -j "$JOBS" >/dev/null
cp "$SSRC/src/sox" "$STAGE/sox"
cp "$SSRC/COPYING" "$STAGE/COPYING.sox.txt"
if [ -f "$SSRC/LICENSE.GPL" ]; then cp "$SSRC/LICENSE.GPL" "$STAGE/LICENSE.GPL.sox.txt"; fi

strip "$STAGE/whisper-server" "$STAGE/sox" 2>/dev/null || true
"$STAGE/sox" --version >/dev/null || fail "the sox just built doesn't run"
"$STAGE/sox" -h | grep -q 'mp3' || fail "the sox just built can't read MP3"
"$STAGE/whisper-server" --help >/dev/null 2>&1 || fail "the whisper-server just built doesn't run"

cat >"$STAGE/SOURCES.txt" <<EOF
The programs in this bundle are built, unmodified, from these sources:

whisper-server: whisper.cpp $WHISPER_VERSION (MIT, LICENSE.whisper.cpp.txt)
  $WHISPER_URL
  sha256 $WHISPER_SHA256

sox: SoX $SOX_VERSION (GPL-2.0-or-later, COPYING.sox.txt), configured by
packaging/voice/build.sh in Mirrin's source
  $SOX_URL
  sha256 $SOX_SHA256
with libmad $MAD_VERSION linked in (GPL-2.0-or-later, COPYING.libmad.txt)
  $MAD_URL
  sha256 $MAD_SHA256

The Mirrin release's source archive holds packaging/voice/build.sh.
EOF

tar -czf "$OUT/$NAME.tar.gz" -C "$WORK" mirrin-voice
say "wrote $OUT/$NAME.tar.gz"
