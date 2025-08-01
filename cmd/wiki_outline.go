package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wsine/feishu2md/core"
	"github.com/Wsine/feishu2md/utils"
)

// 生成Wiki目录树的Markdown文档
func generateWikiOutline(ctx context.Context, client *core.Client, url string) error {
	prefixURL, spaceID, err := utils.ValidateWikiURL(url)
	if err != nil {
		return err
	}

	// 获取Wiki空间名称
	wikiName, err := client.GetWikiName(ctx, spaceID)
	if err != nil {
		return err
	}
	if wikiName == "" {
		return fmt.Errorf("获取Wiki名称失败")
	}

	// 创建输出目录
	if _, err := os.Stat(dlOpts.outputDir); os.IsNotExist(err) {
		if err := os.MkdirAll(dlOpts.outputDir, 0o755); err != nil {
			return err
		}
	}

	// 创建Markdown文件
	sanitizedTitle := utils.SanitizeFileName(wikiName)
	mdName := fmt.Sprintf("%s_目录结构.md", sanitizedTitle)
	outputPath := filepath.Join(dlOpts.outputDir, mdName)

	// 生成Markdown内容
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s 目录结构\n\n", wikiName))
	sb.WriteString(fmt.Sprintf("> 生成时间: %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("> 原Wiki链接: [%s](%s)\n\n", wikiName, url))

	// 递归生成目录树
	err = buildWikiOutline(ctx, client, spaceID, &sb, "", nil, 0, prefixURL, dlOpts.wikiOutlineWithLinks)
	if err != nil {
		return err
	}

	// 写入文件
	if err = os.WriteFile(outputPath, []byte(sb.String()), 0o644); err != nil {
		return err
	}

	fmt.Printf("Wiki目录结构已保存到: %s\n", outputPath)
	return nil
}

// 递归构建Wiki目录树
func buildWikiOutline(
	ctx context.Context,
	client *core.Client,
	spaceID string,
	sb *strings.Builder,
	indent string,
	parentNodeToken *string,
	level int,
	prefixURL string,
	withLinks bool,
) error {
	nodes, err := client.GetWikiNodeList(ctx, spaceID, parentNodeToken)
	if err != nil {
		return err
	}

	for i, node := range nodes {
		// 添加当前节点
		nodePrefix := "- "
		if i == len(nodes)-1 && level > 0 {
			nodePrefix = "- "
		}

		// 根据withLinks参数决定是否生成链接
		var nodeContent string
		if withLinks {
			// 生成带链接的节点
			nodeContent = fmt.Sprintf("%s%s[%s](%s/wiki/%s)", indent, nodePrefix, node.Title, prefixURL, node.NodeToken)
		} else {
			// 生成不带链接的节点
			nodeContent = fmt.Sprintf("%s%s%s", indent, nodePrefix, node.Title)
		}
		sb.WriteString(nodeContent)

		// 添加节点类型标识
		if node.ObjType == "docx" {
			sb.WriteString(" 📄")
		} else if node.HasChild {
			sb.WriteString(" 📁")
		}
		sb.WriteString("\n")

		// 递归处理子节点
		if node.HasChild {
			childIndent := indent + "  "
			if err := buildWikiOutline(ctx, client, spaceID, sb, childIndent, &node.NodeToken, level+1, prefixURL, withLinks); err != nil {
				return err
			}
		}
	}

	return nil
}
