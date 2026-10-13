"use strict";
// Locates the native git-protect binary installed as a platform-specific
// optional dependency and runs it with the caller's stdio, so it behaves
// exactly like the native executable (including as a git remote helper).
const { spawnSync } = require("child_process");
const path = require("path");

function binaryPath() {
  const key = `${process.platform}-${process.arch}`;
  const pkg = `git-protect-${key}`;
  const exe = process.platform === "win32" ? "git-protect.exe" : "git-protect";
  try {
    return path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", exe);
  } catch (e) {
    console.error(
      `git-protect: 没有找到当前平台（${key}）的程序包 ${pkg}。\n` +
        "  请确认安装时没有使用 --no-optional / --omit=optional，然后重新安装：npm install -g git-protect\n" +
        "  支持的平台：win32-x64、darwin-x64、darwin-arm64、linux-x64、linux-arm64"
    );
    process.exit(1);
  }
}

module.exports = function run(prefixArgs) {
  const result = spawnSync(binaryPath(), prefixArgs.concat(process.argv.slice(2)), {
    stdio: "inherit",
  });
  if (result.error) {
    console.error(`git-protect: 无法启动程序：${result.error.message}`);
    process.exit(1);
  }
  if (result.signal) {
    process.kill(process.pid, result.signal);
  }
  process.exit(result.status === null ? 1 : result.status);
};

module.exports.binaryPath = binaryPath;
