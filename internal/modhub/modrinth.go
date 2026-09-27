package modhub

import (
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "net/url"
        "sort"
        "strconv"
        "strings"
        "time"
)

const modrinthAPI = "https://api.modrinth.com/v2"

// ModResult 搜索结果条目。
type ModResult struct {
        ProjectID  string   `json:"project_id"`
        Slug       string   `json:"slug"`
        Title      string   `json:"title"`
        Author     string   `json:"author"`
        Desc       string   `json:"description"`
        Downloads  int64    `json:"downloads"`
        IconURL    string   `json:"icon_url"`
        Categories []string `json:"categories"`
}

// ModVersion 某模组的一个可下载版本。
type ModVersion struct {
        VersionID    string   `json:"version_id"`
        Name         string   `json:"name"`
        Number       string   `json:"version_number"`
        GameVersions []string `json:"game_versions"`
        Loaders      []string `json:"loaders"`
        Published    string   `json:"date_published"`
        FileURL      string   `json:"file_url"`
        Filename     string   `json:"filename"`
        FileSize     int64    `json:"file_size"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func getJSON(endpoint string, out any) error {
        req, err := http.NewRequest("GET", modrinthAPI+endpoint, nil)
        if err != nil {
                return err
        }
        req.Header.Set("User-Agent", "CLink/1.1 (github.com/ccdsyy/CLink)")
        resp, err := httpClient.Do(req)
        if err != nil {
                return err
        }
        defer resp.Body.Close()
        if resp.StatusCode != http.StatusOK {
                return fmt.Errorf("Modrinth HTTP %d", resp.StatusCode)
        }
        b, err := io.ReadAll(resp.Body)
        if err != nil {
                return err
        }
        return json.Unmarshal(b, out)
}

// Search 搜索 Modrinth 模组（按下载量/相关性排序）。
func Search(query string, limit int) ([]ModResult, error) {
        if strings.TrimSpace(query) == "" {
                return nil, nil
        }
        if limit <= 0 || limit > 40 {
                limit = 20
        }
        facet := url.QueryEscape(`[["project_type:mod"]]`)
        var resp struct {
                Hits []ModResult `json:"hits"`
        }
        err := getJSON(fmt.Sprintf("/search?query=%s&limit=%d&index=relevance&facets=%s",
                url.QueryEscape(query), limit, facet), &resp)
        if err != nil {
                return nil, err
        }
        return resp.Hits, nil
}

// Versions 取某模组所有版本（按发布时间倒序），可按游戏版本与加载器过滤。
func Versions(projectID, gameVersion, loader string) ([]ModVersion, error) {
        var raw []struct {
                ID           string   `json:"id"`
                Name         string   `json:"name"`
                Number       string   `json:"version_number"`
                GameVersions []string `json:"game_versions"`
                Loaders      []string `json:"loaders"`
                Published    string   `json:"date_published"`
                Files        []struct {
                        URL      string `json:"url"`
                        Filename string `json:"filename"`
                        Size     int64  `json:"size"`
                        Primary  bool   `json:"primary"`
                } `json:"files"`
        }
        q := ""
        if gameVersion != "" && loader != "" {
                q = fmt.Sprintf("?game_versions=%%5B%%22%s%%22%%5D&loaders=%%5B%%22%s%%22%%5D",
                        url.QueryEscape(gameVersion), url.QueryEscape(loader))
        } else if gameVersion != "" {
                q = fmt.Sprintf("?game_versions=%%5B%%22%s%%22%%5D", url.QueryEscape(gameVersion))
        } else if loader != "" {
                q = fmt.Sprintf("?loaders=%%5B%%22%s%%22%%5D", url.QueryEscape(loader))
        }
        if err := getJSON("/project/"+projectID+"/version"+q, &raw); err != nil {
                return nil, err
        }
        out := make([]ModVersion, 0, len(raw))
        for _, r := range raw {
                v := ModVersion{
                        VersionID: r.ID, Name: r.Name, Number: r.Number,
                        GameVersions: r.GameVersions, Loaders: r.Loaders, Published: r.Published,
                }
                // 优先 primary 文件
                for _, f := range r.Files {
                        if f.Primary {
                                v.FileURL, v.Filename, v.FileSize = f.URL, f.Filename, f.Size
                                break
                        }
                }
                if v.FileURL == "" && len(r.Files) > 0 {
                        v.FileURL, v.Filename, v.FileSize = r.Files[0].URL, r.Files[0].Filename, r.Files[0].Size
                }
                if v.FileURL != "" {
                        out = append(out, v)
                }
        }
        sort.Slice(out, func(i, j int) bool { return out[i].Published > out[j].Published })
        return out, nil
}

// FormatBytes 字节数人性化（8.9 MB）。
func FormatBytes(n int64) string {
        switch {
        case n >= 1<<30:
                return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
        case n >= 1<<20:
                return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
        case n >= 1<<10:
                return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
        default:
                return strconv.FormatInt(n, 10) + " B"
        }
}
