# Generated from the SHA256SUMS of release v0.3.0-preview.5 by
# scripts/release/update-homebrew-formula.sh in https://github.com/GoogleCloudPlatform/scion.
# Do not edit by hand: the next release overwrites this file.
class Scion < Formula
  desc "Orchestrate concurrent LLM code agents in containers"
  homepage "https://github.com/GoogleCloudPlatform/scion"
  version "0.3.0-preview.5"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/GoogleCloudPlatform/scion/releases/download/v0.3.0-preview.5/scion-darwin-arm64.tar.gz"
      sha256 "24296f8ec546bac9d97c9b4cc331b0738a834ba96a7d5aef1b043c74bbaba5e6"
    end
    on_intel do
      url "https://github.com/GoogleCloudPlatform/scion/releases/download/v0.3.0-preview.5/scion-darwin-amd64.tar.gz"
      sha256 "9ee065b5177e77dafbc6581b7cbc6bd2239f7727801149d5c8be31b046b747c0"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/GoogleCloudPlatform/scion/releases/download/v0.3.0-preview.5/scion-linux-arm64.tar.gz"
      sha256 "dd6ae0a2c0500e02eaa45c304b30c55bba95d0e9128a3d7a9206ddc35a54b643"
    end
    on_intel do
      url "https://github.com/GoogleCloudPlatform/scion/releases/download/v0.3.0-preview.5/scion-linux-amd64.tar.gz"
      sha256 "d319a3c05330eb212b75bb3331df7e5f5bf32fde9ee4ee00e922177f6293e4ac"
    end
  end

  def install
    bin.install "scion"
  end

  def caveats
    text = <<~EOS
      To set up Scion, start the workstation server:
        scion server start
      On first start it opens the onboarding wizard, which checks your
      container runtime and sets the image registry.

      If a server was already running, restart it to use the new binary:
        scion server restart

      Container images for this release are published with the release tag,
      for example:
        ghcr.io/homebrew-scion/scion-claude:v0.3.0-preview.5
      This version of Scion pulls the images tagged with its own version.
    EOS
    if OS.linux?
      text += <<~EOS

        On Linux, Homebrew installs its own gcc, once for all formulae, when
        the system glibc is older than 2.39 or libstdc++ is older than
        6.0.33, and its own glibc as well when glibc is older than 2.39.
        Scion is a static binary and does not use them.
        On such hosts you can install the release tarball instead:
          https://github.com/GoogleCloudPlatform/scion/releases/tag/v0.3.0-preview.5
      EOS
    end
    text
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/scion version")
  end
end
