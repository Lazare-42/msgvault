{
  lib,
  buildGoModule,
  gitignoreSource,
  sqlite,
}:
let
  version = "0.16.0";
in
buildGoModule {
  pname = "msgvault";
  inherit version;

  src = gitignoreSource ../.;

  vendorHash = "sha256-QkYNdvNI7kR5JVhdBjnnOaRHcYV3uu7TNNahTgqil9o=";
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
