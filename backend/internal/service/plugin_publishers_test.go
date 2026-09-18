package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type publisherTestLookup map[string]*PluginPublisher

func (p publisherTestLookup) GetTrustedPublisher(_ context.Context, stringKey string) (*PluginPublisher, error) {
	return p[stringKey], nil
}

func publisherArchive(t *testing.T, private ed25519.PrivateKey, keyID string, includeKey bool) []byte {
	t.Helper()
	data := buildTestPluginArchive(t, private, keyID)
	if !includeKey {
		return data
	}
	return rewritePublisherArchive(t, data, func(name string, raw []byte) []byte {
		if name != pluginSignatureFilename {
			return raw
		}
		var sig PluginSignature
		require.NoError(t, json.Unmarshal(raw, &sig))
		sig.PublicKey = base64.StdEncoding.EncodeToString(private[ed25519.SeedSize:])
		out, err := json.Marshal(sig)
		require.NoError(t, err)
		return out
	})
}

func rewritePublisherArchive(t *testing.T, data []byte, change func(string, []byte) []byte) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, file := range reader.File {
		raw, readErr := readPluginZipFile(file, 64<<20)
		require.NoError(t, readErr)
		writeZipEntry(t, writer, file.Name, change(file.Name, raw))
	}
	require.NoError(t, writer.Close())
	return out.Bytes()
}

func TestPluginPublisherFirstImportRequiresExactApproval(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	data := publisherArchive(t, private, "developer-one", true)
	cfg := testPluginConfig(t.TempDir(), false)
	installer := NewPluginPackageInstaller(cfg, PluginHostInfo{Version: "0.1.179"})
	preview, err := installer.Inspect(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, "untrusted", preview.SignatureStatus)
	require.Equal(t, "developer-one", preview.Publisher.KeyID)
	require.Equal(t, publisherFingerprint(private[ed25519.SeedSize:]), preview.Publisher.Fingerprint)
	entries, err := os.ReadDir(cfg.Plugins.DataDir)
	require.NoError(t, err)
	require.Empty(t, entries, "inspection neither extracts nor executes")
	_, err = installer.Install(t.Context(), bytes.NewReader(data), nil)
	require.ErrorContains(t, err, "不受信任")
	for _, approval := range []*PluginPublisherApproval{
		{PackageSHA256: "wrong", Fingerprint: preview.Publisher.Fingerprint},
		{PackageSHA256: preview.PackageSHA256, Fingerprint: "wrong"},
	} {
		_, err = installer.InstallWithApproval(t.Context(), bytes.NewReader(data), nil, approval)
		require.ErrorContains(t, err, "确认内容不一致")
	}
	approval := &PluginPublisherApproval{PackageSHA256: preview.PackageSHA256, Fingerprint: preview.Publisher.Fingerprint}
	installed, err := installer.InstallWithApproval(t.Context(), bytes.NewReader(data), nil, approval)
	require.NoError(t, err)
	require.Equal(t, PluginSignatureTrusted, installed.SignatureStatus)
	require.Equal(t, preview.Publisher, installed.PublisherToTrust)
	require.Empty(t, cfg.Plugins.TrustedPublishers, "request approval must not mutate shared host configuration")
	// 新实例使用已提交的信任记录，兼容未内嵌公钥的旧包。
	fresh := NewPluginPackageInstaller(cfg, PluginHostInfo{Version: "0.1.179"})
	fresh.publishers = publisherTestLookup{"developer-one": installed.PublisherToTrust}
	known, err := fresh.Inspect(t.Context(), bytes.NewReader(publisherArchive(t, private, "developer-one", false)))
	require.NoError(t, err)
	require.Equal(t, PluginSignatureTrusted, known.SignatureStatus)
	_, err = fresh.Install(t.Context(), bytes.NewReader(data), nil)
	require.NoError(t, err)
}

func TestPluginPublisherInspectRejectsTamperingBeforeConsent(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	data := publisherArchive(t, private, "developer-one", true)
	installer := NewPluginPackageInstaller(testPluginConfig(t.TempDir(), false), PluginHostInfo{Version: "0.1.179"})
	for _, target := range []string{pluginManifestFilename, "bin/plugin", "ui/index.html"} {
		t.Run(target, func(t *testing.T) {
			modified := rewritePublisherArchive(t, data, func(name string, raw []byte) []byte {
				if name == target {
					return append(raw, ' ')
				}
				return raw
			})
			_, err := installer.Inspect(t.Context(), bytes.NewReader(modified))
			require.Error(t, err)
		})
	}
	_, err = installer.Inspect(t.Context(), bytes.NewReader(publisherArchive(t, private, "developer-one", false)))
	require.ErrorContains(t, err, "内置发布者公钥")
	_, err = installer.Inspect(t.Context(), bytes.NewReader(buildTestPluginArchive(t, nil, "")))
	require.ErrorContains(t, err, "未签名")
}

func TestPluginPublisherPinsCannotBeReplacedByAnEmbeddedKey(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, replacement, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cfg := testPluginConfig(t.TempDir(), false)
	installer := NewPluginPackageInstaller(cfg, PluginHostInfo{Version: "0.1.179"})
	data := publisherArchive(t, replacement, "developer-one", true)
	preview, err := installer.Inspect(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	approval := &PluginPublisherApproval{PackageSHA256: preview.PackageSHA256, Fingerprint: preview.Publisher.Fingerprint}
	installer.publishers = publisherTestLookup{"developer-one": {KeyID: "developer-one", PublicKey: base64.StdEncoding.EncodeToString(public)}}
	_, err = installer.Inspect(t.Context(), bytes.NewReader(data))
	require.ErrorIs(t, err, ErrPluginPublisherKeyChanged)
	_, err = installer.InstallWithApproval(t.Context(), bytes.NewReader(data), nil, approval)
	require.ErrorIs(t, err, ErrPluginPublisherKeyChanged)
	// 显式配置的信任根优先于数据库记录和包内公钥。
	cfg.Plugins.TrustedPublishers["developer-one"] = base64.StdEncoding.EncodeToString(public)
	installer.publishers = publisherTestLookup{"developer-one": {PublicKey: base64.StdEncoding.EncodeToString(replacement[ed25519.SeedSize:])}}
	_, err = installer.Inspect(t.Context(), bytes.NewReader(data))
	require.ErrorIs(t, err, ErrPluginPublisherKeyChanged)
	// 上游曾保留的发布者 ID 也必须走正常的显式信任流程。
	legacyPackage := publisherArchive(t, private, "sub2api-openai-transport-v1", true)
	preview, err = installer.Inspect(t.Context(), bytes.NewReader(legacyPackage))
	require.NoError(t, err)
	require.Equal(t, "untrusted", preview.SignatureStatus)
	_, err = installer.Install(t.Context(), bytes.NewReader(legacyPackage), nil)
	require.ErrorContains(t, err, "不受信任")
}

func TestPluginPublisherKeepsConfiguredLegacyIdentifiersCompatible(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cfg := testPluginConfig(t.TempDir(), false)
	cfg.Plugins.TrustedPublishers["原有发布者"] = base64.StdEncoding.EncodeToString(public)
	installer := NewPluginPackageInstaller(cfg, PluginHostInfo{Version: "0.1.179"})
	installed, err := installer.Install(t.Context(), bytes.NewReader(publisherArchive(t, private, "原有发布者", false)), nil)
	require.NoError(t, err)
	require.Equal(t, PluginSignatureTrusted, installed.SignatureStatus)
}

func TestPluginPublisherApprovalCannotBeReusedForADifferentPackage(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	data := publisherArchive(t, private, "developer-one", true)
	installer := NewPluginPackageInstaller(testPluginConfig(t.TempDir(), false), PluginHostInfo{Version: "0.1.179"})
	preview, err := installer.Inspect(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	other := rewritePublisherArchive(t, data, func(name string, raw []byte) []byte {
		if name != pluginSignatureFilename {
			return raw
		}
		var sig PluginSignature
		require.NoError(t, json.Unmarshal(raw, &sig))
		sig.KeyID = "developer-two"
		result, e := json.Marshal(sig)
		require.NoError(t, e)
		return result
	})
	_, err = installer.InstallWithApproval(t.Context(), bytes.NewReader(other), nil, &PluginPublisherApproval{PackageSHA256: preview.PackageSHA256, Fingerprint: preview.Publisher.Fingerprint})
	require.ErrorContains(t, err, "确认内容不一致")
}
