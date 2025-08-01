package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/88250/lute"
	"github.com/Wsine/feishu2md/core"
	"github.com/Wsine/feishu2md/utils"
	"github.com/chyroc/lark"
	"github.com/pkg/errors"
)

type DownloadOpts struct {
	outputDir            string
	dump                 bool
	batch                bool
	wiki                 bool
	wikiOutline          bool // 新增：是否只下载wiki目录结构
	wikiOutlineWithLinks bool // 新增：生成wiki目录时是否包含文章链接
}

// DownloadResult 下载结果记录
type DownloadResult struct {
	URL      string    `json:"url"`
	Filename string    `json:"filename"`
	Status   string    `json:"status"` // "success" or "error"
	Error    string    `json:"error,omitempty"`
	Time     time.Time `json:"time"`
}

// BatchDownloadReport 批量下载报告
type BatchDownloadReport struct {
	TotalFiles    int              `json:"total_files"`
	SuccessCount  int              `json:"success_count"`
	ErrorCount    int              `json:"error_count"`
	Results       []DownloadResult `json:"results"`
	StartTime     time.Time        `json:"start_time"`
	EndTime       time.Time        `json:"end_time"`
	Duration      string           `json:"duration"`
}

var dlOpts = DownloadOpts{}
var dlConfig core.Config

// downloadDocumentWithResult 下载文档并返回结果记录
func downloadDocumentWithResult(ctx context.Context, client *core.Client, url string, opts *DownloadOpts) DownloadResult {
	result := DownloadResult{
		URL:    url,
		Time:   time.Now(),
		Status: "error",
	}

	err := downloadDocument(ctx, client, url, opts)
	if err != nil {
		result.Error = err.Error()
		fmt.Printf("Error downloading %s: %v\n", url, err)
	} else {
		result.Status = "success"
		// 尝试从URL中提取文档token来构建文件名
		if docType, docToken, urlErr := utils.ValidateDocumentURL(url); urlErr == nil {
			if docType == "wiki" {
				// 对于wiki页面，需要获取实际的文档信息
				if node, nodeErr := client.GetWikiNodeInfo(ctx, docToken); nodeErr == nil {
					docToken = node.ObjToken
				}
			}
			// 构建文件名 - 使用文档标题作为文件名
			if docx, _, titleErr := client.GetDocxContent(ctx, docToken); titleErr == nil {
				sanitizedTitle := utils.SanitizeFileName(docx.Title)
				result.Filename = fmt.Sprintf("%s.md", sanitizedTitle)
			} else {
				result.Filename = fmt.Sprintf("%s.md", docToken)
			}
		}
	}

	return result
}

func downloadDocument(ctx context.Context, client *core.Client, url string, opts *DownloadOpts) error {
	// Validate the url to download
	docType, docToken, err := utils.ValidateDocumentURL(url)
	if err != nil {
		return err
	}
	fmt.Println("Captured document token:", docToken)

	// for a wiki page, we need to renew docType and docToken first
	if docType == "wiki" {
		node, err := client.GetWikiNodeInfo(ctx, docToken)
		if err != nil {
			return fmt.Errorf("GetWikiNodeInfo err: %v for %v", err, url)
		}
		docType = node.ObjType
		docToken = node.ObjToken
	}
	if docType == "docs" {
		return errors.Errorf(
			`Feishu Docs is no longer supported. ` +
				`Please refer to the Readme/Release for v1_support.`)
	}

	// Process the download
	docx, blocks, err := client.GetDocxContent(ctx, docToken)
	if err != nil {
		return err
	}

	parser := core.NewParser(dlConfig.Output)
	markdown := parser.ParseDocxContent(docx, blocks)

	if !dlConfig.Output.SkipImgDownload {
		for _, imgToken := range parser.ImgTokens {
			localLink, err := client.DownloadImage(
				ctx, imgToken, filepath.Join(opts.outputDir, dlConfig.Output.ImageDir),
			)
			if err != nil {
				return err
			}
			markdown = strings.Replace(markdown, imgToken, localLink, 1)
		}
	}

	// 在markdown开头添加原文档链接
	markdownWithLink := fmt.Sprintf("# %s\n\n> 原文档链接: [%s](%s)\n\n%s", docx.Title, docx.Title, url, markdown)

	// Format the markdown document
	engine := lute.New(func(l *lute.Lute) {
		l.RenderOptions.AutoSpace = true
	})
	result := engine.FormatStr("md", markdownWithLink)

	// Handle the output directory and name
	if _, err := os.Stat(opts.outputDir); os.IsNotExist(err) {
		if err := os.MkdirAll(opts.outputDir, 0o755); err != nil {
			return err
		}
	}

	if dlOpts.dump {
		jsonName := fmt.Sprintf("%s.json", docToken)
		outputPath := filepath.Join(opts.outputDir, jsonName)
		data := struct {
			Document *lark.DocxDocument `json:"document"`
			Blocks   []*lark.DocxBlock  `json:"blocks"`
		}{
			Document: docx,
			Blocks:   blocks,
		}
		pdata := utils.PrettyPrint(data)

		if err = os.WriteFile(outputPath, []byte(pdata), 0o644); err != nil {
			return err
		}
		fmt.Printf("Dumped json response to %s\n", outputPath)
	}

	// Write to markdown file - 使用文档标题作为文件名
	sanitizedTitle := utils.SanitizeFileName(docx.Title)
	mdName := fmt.Sprintf("%s.md", sanitizedTitle)
	outputPath := filepath.Join(opts.outputDir, mdName)
	if err = os.WriteFile(outputPath, []byte(result), 0o644); err != nil {
		return err
	}
	fmt.Printf("Downloaded markdown file to %s\n", outputPath)

	return nil
}

func downloadDocuments(ctx context.Context, client *core.Client, url string) error {
	// Validate the url to download
	folderToken, err := utils.ValidateFolderURL(url)
	if err != nil {
		return err
	}
	fmt.Println("Captured folder token:", folderToken)

	// 初始化批量下载报告
	report := &BatchDownloadReport{
		StartTime: time.Now(),
		Results:   make([]DownloadResult, 0),
	}

	// 使用带缓冲的 channel，避免死锁
	// 缓冲区大小设置为1000，足以处理大多数批量下载场景
	resultChan := make(chan DownloadResult, 1000)
	wg := sync.WaitGroup{}

	// Recursively go through the folder and download the documents
	var processFolder func(ctx context.Context, folderPath, folderToken string) error
	processFolder = func(ctx context.Context, folderPath, folderToken string) error {
		files, err := client.GetDriveFolderFileList(ctx, nil, &folderToken)
		if err != nil {
			return err
		}
		opts := DownloadOpts{outputDir: folderPath, dump: dlOpts.dump, batch: false}
		for _, file := range files {
			if file.Type == "folder" {
				_folderPath := filepath.Join(folderPath, file.Name)
				if err := processFolder(ctx, _folderPath, file.Token); err != nil {
					return err
				}
			} else if file.Type == "docx" {
				// concurrently download the document
				report.TotalFiles++
				wg.Add(1)
				go func(_url string) {
					defer wg.Done()
					result := downloadDocumentWithResult(ctx, client, _url, &opts)
					resultChan <- result
				}(file.URL)
			}
		}
		return nil
	}
	if err := processFolder(ctx, dlOpts.outputDir, folderToken); err != nil {
		return err
	}

	// 等待所有下载完成并收集结果
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集所有下载结果
	for result := range resultChan {
		report.Results = append(report.Results, result)
		if result.Status == "success" {
			report.SuccessCount++
		} else {
			report.ErrorCount++
		}
	}

	// 完成报告
	report.EndTime = time.Now()
	report.Duration = report.EndTime.Sub(report.StartTime).String()

	// 生成并保存下载报告
	if err := generateDownloadReport(report, dlOpts.outputDir); err != nil {
		fmt.Printf("Warning: Failed to generate download report: %v\n", err)
	}

	// 打印下载摘要
	printDownloadSummary(report)

	return nil
}

func downloadWiki(ctx context.Context, client *core.Client, url string) error {
	prefixURL, spaceID, err := utils.ValidateWikiURL(url)
	if err != nil {
		return err
	}

	wikiName, err := client.GetWikiName(ctx, spaceID)
	if err != nil {
		return err
	}
	if wikiName == "" {
		return fmt.Errorf("failed to GetWikiName")
	}
	
	// 使用wiki名称创建根文件夹
	folderPath := filepath.Join(dlOpts.outputDir, utils.SanitizeFileName(wikiName))
	if err := os.MkdirAll(folderPath, 0o755); err != nil {
		return err
	}

	// 初始化批量下载报告
	report := &BatchDownloadReport{
		StartTime: time.Now(),
		Results:   make([]DownloadResult, 0),
	}

	// 使用带缓冲的 channel，避免死锁
	// 缓冲区大小设置为1000，足以处理大多数批量下载场景
	resultChan := make(chan DownloadResult, 1000)

	var maxConcurrency = 10 // Set the maximum concurrency level
	wg := sync.WaitGroup{}
	semaphore := make(chan struct{}, maxConcurrency) // Create a semaphore with the maximum concurrency level

	var downloadWikiNode func(ctx context.Context,
		client *core.Client,
		spaceID string,
		parentPath string,
		parentNodeToken *string) error

	downloadWikiNode = func(ctx context.Context,
		client *core.Client,
		spaceID string,
		folderPath string,
		parentNodeToken *string) error {
		nodes, err := client.GetWikiNodeList(ctx, spaceID, parentNodeToken)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			// 创建当前节点的文件夹路径，使用节点标题
			currentPath := folderPath
			
			// 如果是有子文档的wiki节点，创建以标题命名的文件夹
			if n.HasChild {
				currentPath = filepath.Join(folderPath, utils.SanitizeFileName(n.Title))
				// 确保文件夹存在
				if err := os.MkdirAll(currentPath, 0o755); err != nil {
					return err
				}
				
				// 递归处理子节点
				if err := downloadWikiNode(ctx, client,
					spaceID, currentPath, &n.NodeToken); err != nil {
					return err
				}
			}
			
			// 如果是文档，下载它
			if n.ObjType == "docx" {
				opts := DownloadOpts{outputDir: folderPath, dump: dlOpts.dump, batch: false}
				report.TotalFiles++
				wg.Add(1)
				semaphore <- struct{}{}
				go func(_url string) {
					defer func() {
						wg.Done()
						<-semaphore
					}()
					result := downloadDocumentWithResult(ctx, client, _url, &opts)
					resultChan <- result
				}(prefixURL + "/wiki/" + n.NodeToken)
			}
		}
		return nil
	}

	if err = downloadWikiNode(ctx, client, spaceID, folderPath, nil); err != nil {
		return err
	}

	// 等待所有下载完成并收集结果
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集所有下载结果
	for result := range resultChan {
		report.Results = append(report.Results, result)
		if result.Status == "success" {
			report.SuccessCount++
		} else {
			report.ErrorCount++
		}
	}

	// 完成报告
	report.EndTime = time.Now()
	report.Duration = report.EndTime.Sub(report.StartTime).String()

	// 生成并保存下载报告
	if err := generateDownloadReport(report, dlOpts.outputDir); err != nil {
		fmt.Printf("Warning: Failed to generate download report: %v\n", err)
	}

	// 打印下载摘要
	printDownloadSummary(report)

	return nil
}

// generateDownloadReport 生成下载报告文件
func generateDownloadReport(report *BatchDownloadReport, outputDir string) error {
	reportPath := filepath.Join(outputDir, fmt.Sprintf("report_%s.json", 
		report.StartTime.Format("20060102_150405")))
	
	reportData := utils.PrettyPrint(report)
	return os.WriteFile(reportPath, []byte(reportData), 0o644)
}

// printDownloadSummary 打印下载摘要
func printDownloadSummary(report *BatchDownloadReport) {
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("批量下载完成摘要")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("总文件数: %d\n", report.TotalFiles)
	fmt.Printf("成功下载: %d\n", report.SuccessCount)
	fmt.Printf("下载失败: %d\n", report.ErrorCount)
	fmt.Printf("下载耗时: %s\n", report.Duration)
	
	if report.ErrorCount > 0 {
		fmt.Println("\n失败的文件:")
		for _, result := range report.Results {
			if result.Status == "error" {
				fmt.Printf("  - %s: %s\n", result.URL, result.Error)
			}
		}
	}
	
	if report.SuccessCount > 0 {
		fmt.Println("\n成功下载的文件:")
		for _, result := range report.Results {
			if result.Status == "success" {
				fmt.Printf("  - %s -> %s\n", result.URL, result.Filename)
			}
		}
	}
	fmt.Println(strings.Repeat("=", 50))
}

func handleDownloadCommand(url string) error {
	// Load config
	configPath, err := core.GetConfigFilePath()
	if err != nil {
		return err
	}
	config, err := core.ReadConfigFromFile(configPath)
	if err != nil {
		return err
	}
	dlConfig = *config

	// Instantiate the client
	client := core.NewClient(
		dlConfig.Feishu.AppId, dlConfig.Feishu.AppSecret,
	)
	ctx := context.Background()

	// 如果启用了wikiOutline选项，只生成wiki目录结构
	if dlOpts.wikiOutline {
		return generateWikiOutline(ctx, client, url)
	}

	if dlOpts.batch {
		return downloadDocuments(ctx, client, url)
	}

	if dlOpts.wiki {
		return downloadWiki(ctx, client, url)
	}

	return downloadDocument(ctx, client, url, &dlOpts)
}
