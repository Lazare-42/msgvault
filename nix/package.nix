{
  lib,
  buildGoModule,
  gitignoreSource,
  sqlite,
}:
let
  version = "0.18.0";
in
buildGoModule {
  pname = "msgvault";
  inherit version;

  src = gitignoreSource ../.;

  vendorHash = "sha256-bkeX0ggq73ccGa9Kjlna2hhw15zoQvG6wiAt4gwepXI=";
  proxyVendor = true;

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

  preBuild = ''
    go mod download go.mau.fi/whatsmeow
    gomodcache="$(go env GOMODCACHE)"
    whatsmeow_mod="$gomodcache/go.mau.fi/whatsmeow@v0.0.0-20260630180629-b572e5bcb92b"
    chmod -R u+w "$whatsmeow_mod"
    patch -d "$whatsmeow_mod" -p1 < nix/patches/whatsmeow-clean-failed-pairing-state.patch
  '';

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
