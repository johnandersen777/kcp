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

// kubectl_wasm_fs.ts installs a synchronous globalThis.fs for Go's js/wasm
// syscall package. wasm_exec.js ships an ENOSYS stub when there is no Node
// fs, which makes kubectl fail on kubeconfig stats and discovery-cache
// writes.
//
// Reads are served from the Deno host's real filesystem (the worker must have
// --allow-read). Writes stay in memory: kubectl's discovery cache is per-run
// anyway, and the WASM client should not mutate host files.

type FsCallback = (err: unknown, ...rest: unknown[]) => void;

type StatLike = {
  dev: number;
  ino: number;
  mode: number;
  nlink: number;
  uid: number;
  gid: number;
  rdev: number;
  size: number;
  blksize: number;
  blocks: number;
  atimeMs: number;
  mtimeMs: number;
  ctimeMs: number;
  birthtimeMs: number;
  atime: Date;
  mtime: Date;
  ctime: Date;
  birthtime: Date;
  isDirectory: () => boolean;
  isFile: () => boolean;
  isSymbolicLink: () => boolean;
};

type OpenFile = {
  path: string;
  data: Uint8Array;
  offset: number;
  writable: boolean;
  isDir: boolean;
};

// Node/libuv open constants, matching what syscall/js expects.
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

const files = new Map<number, OpenFile>();
let nextFD = 10;

// In-memory overlay for everything kubectl writes.
const memFiles = new Map<string, Uint8Array>();
const memDirs = new Set<string>();

const stdin = { bytes: new Uint8Array(0), offset: 0 };

/** Sets the bytes returned by reads from fd 0 for the next kubectl run. */
export function setStdin(text: string): void {
  stdin.bytes = new TextEncoder().encode(text);
  stdin.offset = 0;
}

function jsError(code: string): Error {
  const err = new Error(code);
  (err as Error & { code: string }).code = code;
  return err;
}

function errorCode(err: unknown): string {
  if (err instanceof Deno.errors.NotFound) return "ENOENT";
  if (err instanceof Deno.errors.PermissionDenied) return "EACCES";
  if (err instanceof Deno.errors.AlreadyExists) return "EEXIST";
  if (err instanceof Deno.errors.NotADirectory) return "ENOTDIR";
  if (err instanceof Deno.errors.IsADirectory) return "EISDIR";
  const code = (err as { code?: string })?.code;
  if (code === "NotFound") return "ENOENT";
  if (code === "PermissionDenied") return "EACCES";
  if (code === "AlreadyExists") return "EEXIST";
  if (code === "NotADirectory") return "ENOTDIR";
  if (code === "IsADirectory") return "EISDIR";
  return "EIO";
}

function plainStat(mode: number, size: number): StatLike {
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

function denoStat(info: Deno.FileInfo): StatLike {
  const time = (value: Date | null) => value?.getTime() ?? 0;
  const mode = (info.isDirectory ? S_IFDIR : S_IFREG) | (info.mode ?? 0o644);
  return {
    dev: info.dev ?? 0,
    ino: info.ino ?? 0,
    mode,
    nlink: info.nlink ?? 1,
    uid: info.uid ?? 0,
    gid: info.gid ?? 0,
    rdev: info.rdev ?? 0,
    size: info.size ?? 0,
    blksize: info.blksize ?? 4096,
    blocks: info.blocks ?? 0,
    atimeMs: time(info.atime),
    mtimeMs: time(info.mtime),
    ctimeMs: time(info.ctime),
    birthtimeMs: time(info.birthtime),
    atime: info.atime ?? new Date(0),
    mtime: info.mtime ?? new Date(0),
    ctime: info.ctime ?? new Date(0),
    birthtime: info.birthtime ?? new Date(0),
    isDirectory: () => info.isDirectory,
    isFile: () => info.isFile,
    isSymbolicLink: () => info.isSymlink,
  };
}

function statPath(path: string): StatLike {
  if (memFiles.has(path)) {
    return plainStat(S_IFREG | 0o644, memFiles.get(path)!.length);
  }
  if (memDirs.has(path)) {
    return plainStat(S_IFDIR | 0o755, 0);
  }
  return denoStat(Deno.statSync(path));
}

function readPath(path: string): Uint8Array {
  const mem = memFiles.get(path);
  if (mem) return mem;
  return Deno.readFileSync(path);
}

function writePath(path: string): Uint8Array {
  let data = memFiles.get(path);
  if (!data) {
    data = new Uint8Array(0);
    memFiles.set(path, data);
  }
  return data;
}

function parentDirs(path: string): string[] {
  const parts = path.split("/");
  const dirs: string[] = [];
  for (let i = 1; i < parts.length; i++) {
    dirs.push(parts.slice(0, i).join("/") || "/");
  }
  return dirs;
}

function callbackError(cb: FsCallback, err: unknown): void {
  const error = err instanceof Error
    ? jsError(errorCode(err))
    : jsError("EIO");
  cb(error);
}

export function installFsShim(): void {
  const fs = (globalThis as Record<string, unknown>).fs as Record<
    string,
    (...args: never[]) => unknown
  >;

  // Go reads these once in syscall's init.
  (fs as Record<string, unknown>).constants = {
    O_WRONLY,
    O_RDWR,
    O_CREAT,
    O_TRUNC,
    O_APPEND,
    O_EXCL,
    O_DIRECTORY,
  };

  fs.open = ((path: string, flags: number, _mode: number, cb: FsCallback) => {
    try {
      const writable = (flags & (O_WRONLY | O_RDWR)) !== 0;
      if (writable) {
        if (flags & O_EXCL && (memFiles.has(path) || safeStat(path))) {
          cb(jsError("EEXIST"));
          return;
        }
        let data = writePath(path);
        if (flags & O_TRUNC) {
          data = new Uint8Array(0);
          memFiles.set(path, data);
        }
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
      if (isDir && (flags & O_DIRECTORY) === 0) {
        // Directories are opened read-only and then read with readdir.
      }
      const data = isDir ? new Uint8Array(0) : readPath(path);
      const fd = nextFD++;
      files.set(fd, { path, data, offset: 0, writable: false, isDir });
      cb(null, fd);
    } catch (err) {
      callbackError(cb, err);
    }
  }) as never;

  fs.close = ((fd: number, cb: FsCallback) => {
    files.delete(fd);
    cb(null);
  }) as never;

  fs.fstat = ((fd: number, cb: FsCallback) => {
    try {
      if (fd === 0 || fd === 1 || fd === 2) {
        cb(null, plainStat(S_IFCHR | 0o666, 0));
        return;
      }
      const file = files.get(fd);
      if (!file) {
        cb(jsError("EBADF"));
        return;
      }
      if (file.isDir) {
        cb(null, plainStat(S_IFDIR | 0o755, 0));
      } else if (file.writable) {
        cb(null, plainStat(S_IFREG | 0o644, file.data.length));
      } else {
        cb(null, statPath(file.path));
      }
    } catch (err) {
      callbackError(cb, err);
    }
  }) as never;

  fs.stat = ((path: string, cb: FsCallback) => {
    try {
      cb(null, statPath(path));
    } catch (err) {
      callbackError(cb, err);
    }
  }) as never;

  fs.lstat = fs.stat;

  fs.read = ((
    fd: number,
    buffer: Uint8Array,
    offset: number,
    length: number,
    position: number | null,
    cb: FsCallback,
  ) => {
    if (fd === 0) {
      const n = Math.min(length, stdin.bytes.length - stdin.offset);
      if (n <= 0) {
        cb(null, 0);
        return;
      }
      buffer.set(
        stdin.bytes.subarray(stdin.offset, stdin.offset + n),
        offset,
      );
      stdin.offset += n;
      cb(null, n);
      return;
    }
    const file = files.get(fd);
    if (!file) {
      cb(jsError("EBADF"));
      return;
    }
    const start = position === null || position === undefined
      ? file.offset
      : position;
    const n = Math.max(0, Math.min(length, file.data.length - start));
    buffer.set(file.data.subarray(start, start + n), offset);
    file.offset = start + n;
    cb(null, n);
  }) as never;

  fs.write = ((
    fd: number,
    buffer: Uint8Array,
    offset: number,
    length: number,
    position: number | null,
    cb: FsCallback,
  ) => {
    if (fd === 1 || fd === 2) {
      const text = new TextDecoder().decode(
        buffer.subarray(offset, offset + length),
      );
      (fd === 1 ? console.log : console.error)(text);
      cb(null, length);
      return;
    }
    const file = files.get(fd);
    if (!file) {
      cb(jsError("EBADF"));
      return;
    }
    if (!file.writable) {
      cb(jsError("EBADF"));
      return;
    }
    const start = position === null || position === undefined
      ? file.offset
      : position;
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
  }) as never;

  fs.mkdir = ((path: string, _mode: number, cb: FsCallback) => {
    for (const dir of parentDirs(path)) memDirs.add(dir);
    memDirs.add(path);
    cb(null);
  }) as never;

  fs.rmdir = ((path: string, cb: FsCallback) => {
    memDirs.delete(path);
    cb(null);
  }) as never;

  fs.unlink = ((path: string, cb: FsCallback) => {
    memFiles.delete(path);
    cb(null);
  }) as never;

  fs.rename = ((from: string, to: string, cb: FsCallback) => {
    const data = memFiles.get(from);
    if (data) {
      memFiles.delete(from);
      memFiles.set(to, data);
    }
    cb(null);
  }) as never;

  fs.truncate = ((path: string, length: number, cb: FsCallback) => {
    const data = memFiles.get(path) ?? new Uint8Array(0);
    const next = new Uint8Array(length);
    next.set(data.subarray(0, Math.min(length, data.length)));
    memFiles.set(path, next);
    cb(null);
  }) as never;

  fs.ftruncate = ((fd: number, length: number, cb: FsCallback) => {
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
  }) as never;

  fs.fsync = ((_fd: number, cb: FsCallback) => cb(null)) as never;
  fs.chmod = ((_path: string, _mode: number, cb: FsCallback) =>
    cb(null)) as never;
  fs.fchmod = ((_fd: number, _mode: number, cb: FsCallback) =>
    cb(null)) as never;
  fs.chown = ((_path: string, _uid: number, _gid: number, cb: FsCallback) =>
    cb(null)) as never;
  fs.fchown = ((_fd: number, _uid: number, _gid: number, cb: FsCallback) =>
    cb(null)) as never;
  fs.lchown = ((_path: string, _uid: number, _gid: number, cb: FsCallback) =>
    cb(null)) as never;
  fs.utimes = ((
    _path: string,
    _atime: number,
    _mtime: number,
    cb: FsCallback,
  ) => cb(null)) as never;

  fs.readdir = ((path: string, cb: FsCallback) => {
    try {
      const names = new Set<string>();
      for (const name of Deno.readDirSync(path)) names.add(name.name);
      for (const file of memFiles.keys()) {
        if (file.startsWith(path.replace(/\/$/, "") + "/")) {
          const rest = file.slice(path.replace(/\/$/, "").length + 1);
          if (!rest.includes("/")) names.add(rest);
        }
      }
      cb(null, Array.from(names));
    } catch (err) {
      callbackError(cb, err);
    }
  }) as never;

  fs.readlink = ((_path: string, cb: FsCallback) => {
    cb(jsError("EINVAL"));
  }) as never;
}

function safeStat(path: string): boolean {
  try {
    statPath(path);
    return true;
  } catch {
    return false;
  }
}
