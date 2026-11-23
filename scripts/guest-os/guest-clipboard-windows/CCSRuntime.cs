using System;
using System.Collections.Generic;
using System.IO;
using System.Security.Cryptography;
using System.Text.Json;

namespace Cage.GuestClipboard;

internal static class CCSRuntime
{
    internal const string ManifestSha = "ee8a1afc7e08a1e27c582359040571b248d1e932eca27d0880a03597430920c3";
    internal const string MainEntry = "node_modules/@kaitranntt/ccs/dist/ccs.js";

    internal static void Verify(string manifest, string main, string? mainSha)
    {
        var root = Path.GetDirectoryName(manifest)!;
        if (Path.GetFileName(manifest) != "runtime-files.json" || new FileInfo(manifest).Length > 2 << 20 ||
            !Path.GetFullPath(MainEntry, root).Equals(main, StringComparison.OrdinalIgnoreCase))
            throw new InvalidDataException("ccs_manifest_invalid");
        var contents = File.ReadAllBytes(manifest);
        if (Convert.ToHexStringLower(SHA256.HashData(contents)) != ManifestSha)
            throw new InvalidDataException("ccs_manifest_invalid");
        var files = JsonSerializer.Deserialize<Dictionary<string, string>>(contents);
        if (files == null || !files.TryGetValue(MainEntry, out var hash) || hash != mainSha)
            throw new InvalidDataException("ccs_tools_invalid");

        var directories = new Stack<DirectoryInfo>();
        directories.Push(new DirectoryInfo(root));
        while (directories.TryPop(out var current))
        {
            foreach (var entry in current.EnumerateFileSystemInfos())
            {
                if (entry.Attributes.HasFlag(FileAttributes.ReparsePoint))
                    throw new InvalidDataException("ccs_manifest_invalid");
                if (entry is DirectoryInfo directory) { directories.Push(directory); continue; }
                if (entry.FullName.Equals(manifest, StringComparison.Ordinal)) continue;
                var relative = Path.GetRelativePath(root, entry.FullName).Replace(Path.DirectorySeparatorChar, '/');
                if (!files.Remove(relative, out var expected)) throw new InvalidDataException("ccs_tools_changed");
                using var data = File.OpenRead(entry.FullName);
                if (Convert.ToHexStringLower(SHA256.HashData(data)) != expected) throw new InvalidDataException("ccs_tools_changed");
            }
        }
        if (files.Count != 0) throw new InvalidDataException("ccs_tools_changed");
    }
}
