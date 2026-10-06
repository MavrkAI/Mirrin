# Homebrew formula for a tap (e.g. MavrkAI/homebrew-tap → `brew install mavrkai/tap/mirrin`).
# This is a template: scripts/release-brew.sh fills in the version and sha256 values
# from a release's SHA256SUMS, and the release workflow pushes the result to the tap
# when HOMEBREW_TAP_TOKEN is set (docs/maintainers-release.md).
class Mirrin < Formula
  desc "Your own digital twin: a local personal AI that remembers you, talks, and acts for you"
  homepage "https://github.com/MavrkAI/Mirrin"
  version "VERSION"
  # The formula installs the default program, with WhatsApp: it links GPL-3.0
  # code (go.mau.fi/libsignal), so it is distributed as a whole under GPL-3.0.
  # Mirrin's own source is MIT, and the MPL-2.0 files inside keep their
  # licence; `mirrin licenses` lists them all (docs/licensing.md).
  license "GPL-3.0-only"

  on_macos do
    on_arm do
      url "https://github.com/MavrkAI/Mirrin/releases/download/vVERSION/mirrin-darwin-arm64"
      sha256 "SHA_DARWIN_ARM64"
    end
    on_intel do
      url "https://github.com/MavrkAI/Mirrin/releases/download/vVERSION/mirrin-darwin-amd64"
      sha256 "SHA_DARWIN_AMD64"
    end
  end
  on_linux do
    on_arm do
      url "https://github.com/MavrkAI/Mirrin/releases/download/vVERSION/mirrin-linux-arm64"
      sha256 "SHA_LINUX_ARM64"
    end
    on_intel do
      url "https://github.com/MavrkAI/Mirrin/releases/download/vVERSION/mirrin-linux-amd64"
      sha256 "SHA_LINUX_AMD64"
    end
  end

  depends_on "sox" => :recommended
  depends_on "whisper-cpp" => :recommended

  def install
    bin.install Dir["mirrin-*"].first => "mirrin"
  end

  # No `brew services` entry: it would run a second, headless twin without your
  # shell's API keys. `mirrin service install` sets up the one that belongs in
  # your session (the menu bar on macOS).
  def caveats
    <<~EOS
      To start:
        mirrin chat                # works with zero config; `mirrin init` if you'd rather be asked
        mirrin service install     # keep it running: menu bar, wake word, channels
    EOS
  end

  test do
    assert_match "mirrin", shell_output("#{bin}/mirrin version")
  end
end
