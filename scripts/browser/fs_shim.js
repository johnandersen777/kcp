/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Installs a synchronous globalThis.fs for Go's js/wasm syscall package in
// the browser. There is no host filesystem: everything kubectl needs (the
// discovery cache, a kubeconfig path it probes) lives in memory. fd 0 is fed
// with the stdin sent with each run, so `kubectl create -f -` can work when a
// caller supplies stdin.

const O_WRONLY = 1;
const O_RDWR = 2;
const O_CREAT = 64;
const O_EXCL = 128;
const O_TRUNC = 512;
const O_APPEND = 1024;
const O_DIRECTORY = 65536;

const S_IFDIR = 0o040000;
const S_IFREG = 0o100000;
const S_IFCHR = 0o020000;

const files = new Map();
let nextFD = 10;

const memFiles = new Map();
const memDirs = new Set(["/"]);

const stdin = { bytes: new Uint8Array(0), offset: 0 };

let cwd = "/";
let outputSink = null;

export function setStdin(text) {
  stdin.bytes = new TextEncoder().encode(text ?? "");
  stdin.offset = 0;
}

/** Resolves relative paths against the Go process working directory. */
export function setCwd(path) {
  cwd = normalizePath(path);
}

export function getCwd() {
  return cwd;
}

/** Routes writes to fd 1/2 into a per-run capture instead of the console. */
export function setOutputSink(sink) {
  outputSink = sink;
}

export function snapshotFiles() {
  const snapshot = {};
  const decoder = new TextDecoder();
  for (const [path, data] of memFiles) {
    snapshot[path] = decoder.decode(data);
  }
  return snapshot;
}

export function clearMemoryFs() {
  memFiles.clear();
  memDirs.clear();
  memDirs.add("/");
  files.clear();
  nextFD = 10;
}

export function writeMemoryFile(path, content) {
  const resolved = normalizePath(path);
  ensureParents(resolved);
  memFiles.set(resolved, new TextEncoder().encode(content));
}

export function makeMemoryDir(path) {
  const resolved = normalizePath(path);
  ensureParents(resolved);
  memDirs.add(resolved);
}

function normalizePath(path) {
  const absolute = String(path).startsWith("/")
    ? String(path)
    : `${cwd.replace(/\/$/, "")}/${path}`;
  const parts = [];
  for (const part of absolute.split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") parts.pop();
    else parts.push(part);
  }
  return `/${parts.join("/")}`;
}

function ensureParents(path) {
  const parts = path.split("/").filter(Boolean);
  for (let i = 1; i < parts.length; i++) {
    memDirs.add(`/${parts.slice(0, i).join("/")}`);
  }
  memDirs.add("/");
}

function jsError(code) {
  const err = new Error(code);
  err.code = code;
  return err;
}

function plainStat(mode, size) {
  const zero = new Date(0);
  return {
    dev: 0,
    ino: 0,
    mode,
    nlink: 1,
    uid: 0,
    gid: 0,
    rdev: 0,
    size,
    blksize: 4096,
    blocks: Math.ceil(size / 512),
    atimeMs: 0,
    mtimeMs: 0,
    ctimeMs: 0,
    birthtimeMs: 0,
    atime: zero,
    mtime: zero,
    ctime: zero,
    birthtime: zero,
    isDirectory: () => (mode & S_IFDIR) !== 0,
    isFile: () => (mode & S_IFREG) !== 0,
    isSymbolicLink: () => false,
  };
}

function statPath(path) {
  path = normalizePath(path);
  if (memFiles.has(path)) {
    return plainStat(S_IFREG | 0o644, memFiles.get(path).length);
  }
  if (memDirs.has(path)) {
    return plainStat(S_IFDIR | 0o755, 0);
  }
  throw jsError("ENOENT");
}

function parentDirs(path) {
  const parts = path.split("/");
  const dirs = [];
  for (let i = 1; i < parts.length; i++) dirs.push(parts.slice(0, i).join("/") || "/");
  return dirs;
}

function callbackError(cb, err) {
  cb(err instanceof Error && err.code ? jsError(err.code) : jsError("EIO"));
}

export function installFsShim() {
  const fs = globalThis.fs;

  // Go reads these once in syscall's init.
  fs.constants = {
    O_WRONLY,
    O_RDWR,
    O_CREAT,
    O_TRUNC,
    O_APPEND,
    O_EXCL,
    O_DIRECTORY,
  };

  fs.open = (path, flags, _mode, cb) => {
    path = normalizePath(path);
    try {
      const writable = (flags & (O_WRONLY | O_RDWR)) !== 0;
      if (writable) {
        if ((flags & O_EXCL) && (memFiles.has(path) || memDirs.has(path))) {
          cb(jsError("EEXIST"));
          return;
        }
        let data = memFiles.get(path) ?? new Uint8Array(0);
        if (flags & O_TRUNC) data = new Uint8Array(0);
        memFiles.set(path, data);
        const fd = nextFD++;
        files.set(fd, {
          path,
          data,
          offset: flags & O_APPEND ? data.length : 0,
          writable: true,
          isDir: false,
        });
        cb(null, fd);
        return;
      }

      const info = statPath(path);
      const isDir = info.isDirectory();
      const data = isDir ? new Uint8Array(0) : memFiles.get(path);
      const fd = nextFD++;
      files.set(fd, { path, data, offset: 0, writable: false, isDir });
      cb(null, fd);
    } catch (err) {
      callbackError(cb, err);
    }
  };

  fs.close = (fd, cb) => {
    files.delete(fd);
    cb(null);
  };

  fs.fstat = (fd, cb) => {
    if (fd === 0 || fd === 1 || fd === 2) {
      cb(null, plainStat(S_IFCHR | 0o666, 0));
      return;
    }
    const file = files.get(fd);
    if (!file) {
      cb(jsError("EBADF"));
      return;
    }
    if (file.isDir) cb(null, plainStat(S_IFDIR | 0o755, 0));
    else if (file.writable) cb(null, plainStat(S_IFREG | 0o644, file.data.length));
    else cb(null, plainStat(S_IFREG | 0o644, file.data.length));
  };

  fs.stat = (path, cb) => {
    try {
      cb(null, statPath(path));
    } catch (err) {
      callbackError(cb, err);
    }
  };

  fs.lstat = fs.stat;

  fs.read = (fd, buffer, offset, length, position, cb) => {
    if (fd === 0) {
      const n = Math.min(length, stdin.bytes.length - stdin.offset);
      if (n <= 0) {
        cb(null, 0);
        return;
      }
      buffer.set(stdin.bytes.subarray(stdin.offset, stdin.offset + n), offset);
      stdin.offset += n;
      cb(null, n);
      return;
    }
    const file = files.get(fd);
    if (!file) {
      cb(jsError("EBADF"));
      return;
    }
    const start = position === null || position === undefined ? file.offset : position;
    const n = Math.max(0, Math.min(length, file.data.length - start));
    buffer.set(file.data.subarray(start, start + n), offset);
    file.offset = start + n;
    cb(null, n);
  };

  fs.write = (fd, buffer, offset, length, position, cb) => {
    if (fd === 1 || fd === 2) {
      const text = new TextDecoder().decode(buffer.subarray(offset, offset + length));
      if (outputSink) {
        if (fd === 1) outputSink.stdout += text;
        else outputSink.stderr += text;
      } else {
        (fd === 1 ? console.log : console.error)(text);
      }
      cb(null, length);
      return;
    }
    const file = files.get(fd);
    if (!file || !file.writable) {
      cb(jsError("EBADF"));
      return;
    }
    const start = position === null || position === undefined ? file.offset : position;
    const end = start + length;
    if (end > file.data.length) {
      const grown = new Uint8Array(end);
      grown.set(file.data);
      file.data = grown;
      memFiles.set(file.path, grown);
    }
    file.data.set(buffer.subarray(offset, offset + length), start);
    file.offset = end;
    cb(null, length);
  };

  fs.mkdir = (path, _mode, cb) => {
    path = normalizePath(path);
    for (const dir of parentDirs(path)) memDirs.add(dir);
    memDirs.add(path);
    cb(null);
  };

  fs.rmdir = (path, cb) => {
    memDirs.delete(normalizePath(path));
    cb(null);
  };

  fs.unlink = (path, cb) => {
    memFiles.delete(normalizePath(path));
    cb(null);
  };

  fs.rename = (from, to, cb) => {
    from = normalizePath(from);
    to = normalizePath(to);
    const data = memFiles.get(from);
    if (data) {
      memFiles.delete(from);
      memFiles.set(to, data);
    }
    cb(null);
  };

  fs.truncate = (path, length, cb) => {
    path = normalizePath(path);
    const data = memFiles.get(path) ?? new Uint8Array(0);
    const next = new Uint8Array(length);
    next.set(data.subarray(0, Math.min(length, data.length)));
    memFiles.set(path, next);
    cb(null);
  };

  fs.ftruncate = (fd, length, cb) => {
    const file = files.get(fd);
    if (!file) {
      cb(jsError("EBADF"));
      return;
    }
    const next = new Uint8Array(length);
    next.set(file.data.subarray(0, Math.min(length, file.data.length)));
    file.data = next;
    memFiles.set(file.path, next);
    cb(null);
  };

  fs.fsync = (_fd, cb) => cb(null);
  fs.chmod = (_path, _mode, cb) => cb(null);
  fs.fchmod = (_fd, _mode, cb) => cb(null);
  fs.chown = (_path, _uid, _gid, cb) => cb(null);
  fs.fchown = (_fd, _uid, _gid, cb) => cb(null);
  fs.lchown = (_path, _uid, _gid, cb) => cb(null);
  fs.utimes = (_path, _atime, _mtime, cb) => cb(null);

  fs.readdir = (path, cb) => {
    const base = normalizePath(path).replace(/\/$/, "");
    const names = new Set();
    for (const file of memFiles.keys()) {
      if (file.startsWith(base + "/")) {
        const rest = file.slice(base.length + 1);
        if (!rest.includes("/")) names.add(rest);
      }
    }
    cb(null, Array.from(names));
  };

  fs.readlink = (_path, cb) => cb(jsError("EINVAL"));
}
