package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var ErrPluginPublisherKeyChanged = errors.New("同名插件发布者的公钥与已信任记录冲突，不能自动替换")
var pluginPublisherIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type PluginPublisher struct {
	KeyID       string `json:"key_id"`
	PublicKey   string `json:"-"`
	Fingerprint string `json:"fingerprint"`
}

type PluginPublisherLookup interface {
	GetTrustedPublisher(context.Context, string) (*PluginPublisher, error)
}

type PluginPublisherRepository interface {
	PluginPublisherLookup
	InstallWithPublisher(context.Context, *PluginInstallation, []PluginBinding, *PluginPublisher) (*PluginInstallation, error)
}

type PluginPublisherVersionRepository interface {
	SwapVersionWithPublisher(context.Context, *PluginInstallation, *PluginInstallation, *PluginPublisher) (*PluginInstallation, error)
}

type PluginPublisherApproval struct {
	PackageSHA256 string `json:"package_sha256"`
	Fingerprint   string `json:"publisher_fingerprint"`
}

type PluginPackageInspection struct {
	Manifest         PluginManifest      `json:"manifest"`
	Compatibility    PluginCompatibility `json:"compatibility"`
	PackageSHA256    string              `json:"package_sha256"`
	SignatureStatus  string              `json:"signature_status"`
	Publisher        *PluginPublisher    `json:"publisher,omitempty"`
	RuntimeIsolation string              `json:"runtime_isolation"`
}

func publisherFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// 包内公钥仅用于验签；首次信任必须经过绑定包哈希的管理员确认与安装事务。
func (i *PluginPackageInstaller) checkPackageSignature(ctx context.Context, file *zip.File, manifestRaw []byte, allowUnknown bool) (string, *PluginPublisher, error) {
	if file == nil {
		if i.cfg.Plugins.AllowUnsigned {
			return PluginSignatureUnsigned, nil, nil
		}
		return "", nil, errors.New("生产配置不允许安装未签名插件")
	}
	raw, err := readPluginZipFile(file, 64*1024)
	if err != nil {
		return "", nil, fmt.Errorf("读取插件签名: %w", err)
	}
	var sig PluginSignature
	if err := json.Unmarshal(raw, &sig); err != nil {
		return "", nil, fmt.Errorf("解析插件签名: %w", err)
	}
	if sig.Algorithm != "ed25519" || strings.TrimSpace(sig.KeyID) == "" {
		return "", nil, errors.New("插件签名算法或发布者 ID 无效")
	}
	encoded := trustedPluginPublisherKey(i.cfg, sig.KeyID)
	_, configured := i.cfg.Plugins.TrustedPublishers[sig.KeyID]
	if encoded == "" && configured {
		return "", nil, errors.New("受信任发布者公钥无效")
	}
	if encoded == "" && i.publishers != nil {
		stored, err := i.publishers.GetTrustedPublisher(ctx, sig.KeyID)
		if err != nil {
			return "", nil, fmt.Errorf("读取发布者信任记录: %w", err)
		}
		if stored != nil {
			encoded = stored.PublicKey
			if encoded == "" {
				return "", nil, errors.New("已保存发布者公钥无效")
			}
		}
	}
	trusted := encoded != ""
	if !trusted {
		if !pluginPublisherIDPattern.MatchString(sig.KeyID) {
			return "", nil, errors.New("新发布者 ID 须为 1–128 位英文数字、点、下划线或横线")
		}
		if sig.PublicKey == "" || !allowUnknown {
			return "", nil, fmt.Errorf("插件发布者密钥不受信任: %s；首次安装需使用内置发布者公钥的包并确认信任", sig.KeyID)
		}
		encoded = sig.PublicKey
	}
	public, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(public) != ed25519.PublicKeySize {
		return "", nil, errors.New("插件发布者公钥无效")
	}
	if sig.PublicKey != "" {
		embedded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(sig.PublicKey))
		if decodeErr != nil || !bytes.Equal(public, embedded) {
			return "", nil, ErrPluginPublisherKeyChanged
		}
	}
	signature, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil || !ed25519.Verify(public, manifestRaw, signature) {
		return "", nil, errors.New("插件签名校验失败")
	}
	publisher := &PluginPublisher{KeyID: sig.KeyID, PublicKey: base64.StdEncoding.EncodeToString(public), Fingerprint: publisherFingerprint(public)}
	if trusted {
		return PluginSignatureTrusted, publisher, nil
	}
	return "untrusted", publisher, nil
}

func (i *PluginPackageInstaller) inspectInstallArchive(ctx context.Context, archive *zip.Reader, artifactSHA string, approval *PluginPublisherApproval) (PluginManifest, string, *PluginPublisher, error) {
	manifest, raw, entries, err := i.inspectArchiveMetadata(archive)
	if err != nil {
		return PluginManifest{}, "", nil, err
	}
	status, publisher, err := i.checkPackageSignature(ctx, entries[pluginSignatureFilename], raw, approval != nil)
	if err != nil {
		return PluginManifest{}, "", nil, err
	}
	if approval != nil && (publisher == nil || approval.PackageSHA256 != artifactSHA || approval.Fingerprint != publisher.Fingerprint) {
		return PluginManifest{}, "", nil, errors.New("插件文件或发布者与确认内容不一致，请重新选择插件包")
	}
	if status == "untrusted" {
		// 安装器校验全部声明文件后，仓库将信任记录与安装记录原子提交。
		return manifest, PluginSignatureTrusted, publisher, nil
	}
	return manifest, status, nil, nil
}

func (i *PluginPackageInstaller) Inspect(ctx context.Context, reader io.Reader) (*PluginPackageInspection, error) {
	if i == nil || i.cfg == nil || i.cfg.Plugins.MaxUploadBytes <= 0 {
		return nil, errors.New("插件安装器未配置")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, i.cfg.Plugins.MaxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > i.cfg.Plugins.MaxUploadBytes {
		return nil, errors.New("插件包超过上传大小限制")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("插件包不是有效的 ZIP")
	}
	manifest, raw, entries, err := i.inspectArchiveMetadata(archive)
	if err != nil {
		return nil, err
	}
	status, publisher, err := i.checkPackageSignature(ctx, entries[pluginSignatureFilename], raw, true)
	if err != nil {
		return nil, err
	}
	var extracted int64
	for name, expected := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, err := entries[name].Open()
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		n, copyErr := io.Copy(hash, io.LimitReader(entry, i.cfg.Plugins.MaxUncompressedBytes-extracted+1))
		closeErr := entry.Close()
		if copyErr != nil || closeErr != nil {
			return nil, fmt.Errorf("读取插件文件失败: %s", name)
		}
		extracted += n
		if extracted > i.cfg.Plugins.MaxUncompressedBytes {
			return nil, errors.New("插件包实际解压体积超过限制")
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return nil, fmt.Errorf("插件文件哈希不匹配: %s", name)
		}
	}
	sum := sha256.Sum256(data)
	isolation := "process"
	if manifest.SchemaVersion == 2 {
		isolation = i.cfg.Plugins.V2Sandbox.WithDefaults().Mode
	}
	return &PluginPackageInspection{Manifest: manifest, Compatibility: EvaluatePluginCompatibility(manifest, i.hostInfo), PackageSHA256: hex.EncodeToString(sum[:]), SignatureStatus: status, Publisher: publisher, RuntimeIsolation: isolation}, nil
}

func (m *PluginManager) InspectPackage(ctx context.Context, reader io.Reader) (*PluginPackageInspection, error) {
	return m.installer.Inspect(ctx, reader)
}
