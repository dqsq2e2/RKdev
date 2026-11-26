package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var githubReleasesPath = regexp.MustCompile(`^/repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/releases$`)
var githubVersionSuffix = regexp.MustCompile(`^([0-9]{4})-v([1-9][0-9]?)\.([1-9][0-9]?)-r([1-9][0-9]*)$`)

type githubFirmwareRelease struct {
	Tag        string               `json:"tag_name"`
	Draft      bool                 `json:"draft"`
	Prerelease bool                 `json:"prerelease"`
	Assets     []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest"`
}

func githubReleaseOrder(suffix string) ([]int, string) {
	match := githubVersionSuffix.FindStringSubmatch(suffix)
	if match == nil {
		return nil, ""
	}
	order := make([]int, 4)
	for i := range order {
		var err error
		order[i], err = strconv.Atoi(match[i+1])
		if err != nil {
			return nil, ""
		}
	}
	day := time.Date(order[0], time.Month(order[1]), order[2], 0, 0, 0, 0, time.UTC)
	if int(day.Month()) != order[1] || day.Day() != order[2] {
		return nil, ""
	}
	return order, strings.SplitN(suffix, "-v", 2)[1]
}

func selectGitHubRelease(releases []githubFirmwareRelease, repo string, l runtimeLayout) (onlineRelease, error) {
	var latest onlineRelease
	var best []int
	prefix := l.BoardID + "-" + l.Profile + "-"
	for _, release := range releases {
		suffix, ok := strings.CutPrefix(release.Tag, prefix)
		if !ok || release.Draft || release.Prerelease {
			continue
		}
		order, version := githubReleaseOrder(suffix)
		if slices.Compare(order, best) <= 0 {
			continue
		}
		name := fmt.Sprintf("%s-%s-%s-%s.zip", l.BoardID, l.Revision, l.Profile, version)
		for _, asset := range release.Assets {
			digest, ok := strings.CutPrefix(asset.Digest, "sha256:")
			if asset.Name == name && asset.State == "uploaded" && ok && layoutIDRE.MatchString(digest) &&
				asset.Size > 0 && asset.Size <= l.Capacity+(1<<20) {
				best = order
				latest = onlineRelease{Version: version, Image: releaseImage{name, asset.Size, digest},
					download: "https://github.com/" + repo + "/releases/download/" + release.Tag + "/" + name}
				break
			}
		}
	}
	if latest.download == "" {
		return latest, fmt.Errorf("升级源没有匹配此板型和启动介质的固件 ZIP")
	}
	return latest, nil
}

func (l runtimeLayout) fetchGitHubRelease(ctx context.Context, u *url.URL) (onlineRelease, error) {
	api := *u
	query := api.Query()
	query.Set("per_page", "100")
	api.RawQuery = query.Encode()
	resp, err := getRelease(ctx, api.String())
	if err != nil {
		return onlineRelease{}, err
	}
	defer resp.Body.Close()
	var releases []githubFirmwareRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return onlineRelease{}, fmt.Errorf("无法读取 GitHub 发布列表：%w", err)
	}
	repo := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/repos/"), "/releases")
	return selectGitHubRelease(releases, repo, l)
}

func unpackFirmwareZIP(input io.ReaderAt, size int64, output io.Writer, l runtimeLayout, version string) (releaseManifest, int64, error) {
	var manifest releaseManifest
	bundle, err := zip.NewReader(input, size)
	if err != nil {
		return manifest, 0, fmt.Errorf("无法读取固件 ZIP：%w", err)
	}
	base := strings.Join([]string{l.BoardID, l.Revision, l.Profile, version}, "-")
	var info, image *zip.File
	for _, f := range bundle.File {
		switch {
		case f.Name == base+".json" && f.Mode().IsRegular() && info == nil:
			info = f
		case f.Name == base+".img" && f.Mode().IsRegular() && image == nil:
			image = f
		default:
			return manifest, 0, fmt.Errorf("固件 ZIP 含有意外文件")
		}
	}
	if info == nil || info.UncompressedSize64 > 65536 || image == nil || image.UncompressedSize64 != uint64(l.Capacity) {
		return manifest, 0, fmt.Errorf("固件 ZIP 缺少配套镜像或清单")
	}
	meta, err := info.Open()
	if err != nil {
		return manifest, 0, err
	}
	defer meta.Close()
	manifest, err = readReleaseManifest(meta, l)
	if err != nil {
		return manifest, 0, err
	}
	if manifest.Image.Name != path.Base(image.Name) || manifest.Sources.Version != version {
		return manifest, 0, fmt.Errorf("固件 ZIP 与升级清单不匹配")
	}
	raw, err := image.Open()
	if err != nil {
		return manifest, 0, err
	}
	defer raw.Close()
	written, err := io.Copy(output, io.LimitReader(raw, l.Capacity+1))
	if err != nil {
		return manifest, written, fmt.Errorf("解压升级镜像失败：%w", err)
	}
	if written != l.Capacity {
		return manifest, written, fmt.Errorf("解压后的镜像大小不匹配")
	}
	return manifest, written, nil
}
