{
  lib,
  buildGoModule,
  fetchurl,
  bun2nix,
  gitignoreSource,
  nodejs,
  runCommand,
  sqlite,
}:
let
  version = "0.22.0";
  kitUIURL = "https://github.com/kenn-io/kit-ui/archive/1bd3bb05ac0fbb0dba8b6372ba53106380e64632.tar.gz";
  kitUI = fetchurl {
    url = kitUIURL;
    hash = "sha512-N2c0xfkx6uZTSfdslAZqdpk0/AUQ7eadMxoBzW8SKT6WI4PXXyOmA1Vdd6iJx9x9LSXkxHQwMISvCQLFxoZhgg==";
  };
in
buildGoModule {
  pname = "msgvault";
  inherit version;

  src = gitignoreSource ../.;

  vendorHash = "sha256-sdIziX/FkUbAoRQfzkdmopyY6EaEYzigJGJdwGjPyUc=";
  proxyVendor = true;

  # Bun's copyfile backend can install incomplete packages when fetchBunDeps'
  # cache entries are backed by symlinks. Materialize the cache as regular
  # files before the sandboxed install consumes it.
  bunDeps =
    let
      base = bun2nix.fetchBunDeps { bunNix = ../web/bun.nix; };
    in
    runCommand "msgvault-bun-deps" { } ''
      mkdir -p "$out/share/bun-cache"
      cp -RL ${base}/share/bun-cache/. "$out/share/bun-cache/"
      chmod -R u+w "$out/share/bun-cache"
    '';
  bunRoot = "web";
  # Bun 1.4 re-fetches URL tarballs despite a populated bun2nix cache.
  # Use the exact lockfile-verified tarball as a local dependency in the sandbox.
  preBunNodeModulesInstallPhase = ''
    substituteInPlace package.json --replace-fail "${kitUIURL}" "file:${kitUI}"
    substituteInPlace bun.lock --replace-fail "${kitUIURL}" "${kitUI}"
  '';
  bunInstallFlags = [
    "--linker=hoisted"
    "--backend=copyfile"
  ];
  # The frontend dependencies do not need install scripts. A second Linux Bun
  # install can remove package binaries created by the offline install.
  dontRunLifecycleScripts = true;
  dontUseBunBuild = true;
  dontUseBunCheck = true;
  dontUseBunInstall = true;

  nativeBuildInputs = [
    bun2nix.hook
    nodejs
  ];
  overrideModAttrs = _: previous: {
    nativeBuildInputs = builtins.filter (
      input: (input.name or "") != "bun2nix-hook"
    ) previous.nativeBuildInputs;
    preBuild = "";
  };

  preBuild = ''
    go mod download go.mau.fi/whatsmeow
    gomodcache="$(go env GOMODCACHE)"
    whatsmeow_mod="$gomodcache/go.mau.fi/whatsmeow@v0.0.0-20261017175727-b2b1e71d45e2"
    chmod -R u+w "$whatsmeow_mod"
    patch -d "$whatsmeow_mod" -p1 < nix/patches/whatsmeow-clean-failed-pairing-state.patch

    pushd web
    bun run generate
    bun node_modules/vite/bin/vite.js build
    popd
    mkdir -p internal/web/dist
    find internal/web/dist -mindepth 1 -maxdepth 1 ! -name stub.html -exec rm -rf {} +
    cp -R web/dist/. internal/web/dist/
    bun scripts/check-web-assets.mjs
  '';

  subPackages = [ "cmd/msgvault" ];

  # mattn/go-sqlite3, marcboeker/go-duckdb, and asg017/sqlite-vec-go-bindings
  # all link C code. buildGoModule defaults CGO_ENABLED to 1, but be explicit.
  env.CGO_ENABLED = 1;

  # sqlite-vec-go-bindings does `#include "sqlite3.h"` but ships no sqlite
  # source — provide the system header via buildInputs. flake.nix asserts a
  # minimum SQLite version before passing this package in.
  buildInputs = [ sqlite ];

  tags = [
    "fts5"
    "sqlite_vec"
  ];

  ldflags = [
    "-s"
    "-w"
    "-X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=${version}"
  ];

  doCheck = false;

  meta = {
    description = "Offline Gmail archive with full-text search";
    homepage = "https://github.com/kenn-io/msgvault";
    license = lib.licenses.asl20;
    mainProgram = "msgvault";
  };
}
