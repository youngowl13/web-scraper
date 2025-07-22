// ACMA Automechanika New Delhi – FULL detail scraper
// ---------------------------------------------------
// Scrapes every exhibitor's detail page (opened after clicking a tile) and
// exports Name, Address, Phone, Email, Website into XLSX/CSV.
//
// Usage:
//   go run main.go -out exhibitors.xlsx                 # default XLSX output
//   go run main.go -out exhibitors.csv                  # CSV output
//   go run main.go -headful -dump-html list.html        # debug: show browser & dump list DOM
//   go run main.go -concurrency 4 -timeout 240s         # speed/timeout tuning
//
// Deps:
//   go get github.com/chromedp/chromedp
//   go get github.com/PuerkitoBio/goquery
//   go get github.com/xuri/excelize/v2
//
// NOTE:
// * The site is JS-driven. We render with Chromedp, scroll/click "load more" until all tiles show.
// * Then we collect each tile link and open it to extract the address block you showed in the screenshot.
// * If selectors break, inspect one detail page in DevTools and tweak `detailSel`.
// * Respect robots.txt / Terms of Use.
// ---------------------------------------------------
package main

import (
    "context"
    "encoding/csv"
    "errors"
    "flag"
    "fmt"
    "log"
    "os"
    "regexp"
    "runtime"
    "strings"
    "sync"
    "time"

    "github.com/PuerkitoBio/goquery"
    "github.com/chromedp/cdproto/emulation"
    "github.com/chromedp/chromedp"
    "github.com/xuri/excelize/v2"
)

const startURL = "https://acma-automechanika-newdelhi.in.messefrankfurt.com/newdelhi/en/exhibitor-search.html"

// ----------- Selectors -------------
// Tile/list selectors: pick each exhibitor card and a link to its detail page
var listSel = struct {
    card string
    link string
}{
    card: ".exhibitor-tile, .mf-exhibitor-tile, [data-exhibitor-tile]",
    link: "a[href]", // first anchor in card
}

// Detail page selectors (address block etc.)
// Inspect and tweak if needed.
var detailSel = struct {
    root    string
    name    string
    address string
    phone   string
    email   string
    website string
}{
    root:    ".exhibitor-detail, .c-exhibitor-details, .mf-exhibitor-detail, main",
    name:    ".exhibitor-name, .c-exhibitor__name, h1, h2, h3",
    address: ".exhibitor-address, .c-exhibitor__address, .address",
    phone:   "a[href^=tel], .phone",
    email:   "a[href^=mailto]",
    website: "a[href^=http]",
}

var (
    phoneRx = regexp.MustCompile(`(?i)(tel\.?|phone\:?)[^0-9+]*([+]?\d[\d\s()/-]{5,})`)
    emailRx = regexp.MustCompile(`[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}`)
)

type Exhibitor struct {
    Name    string
    Address string
    Phone   string
    Email   string
    Website string
    Link    string // detail URL for traceability
}

func main() {
    out := flag.String("out", "exhibitors.xlsx", "output file (.xlsx or .csv)")
    headful := flag.Bool("headful", false, "run Chrome with UI (debug)")
    dumpHTML := flag.String("dump-html", "", "dump list-page HTML to this file")
    concurrency := flag.Int("concurrency", runtime.NumCPU(), "parallel detail scrapers")
    timeout := flag.Duration("timeout", 180*time.Second, "overall timeout")
    flag.Parse()

    html, links, err := renderListAndGetLinks(*headful, *timeout)
    if err != nil {
        log.Fatalf("list render error: %v", err)
    }
    if *dumpHTML != "" {
        _ = os.WriteFile(*dumpHTML, []byte(html), 0644)
    }
    if len(links) == 0 {
        log.Fatal("no exhibitor links found – adjust listSel/link extraction")
    }
    links = dedupe(links)
    log.Printf("Found %d detail links", len(links))

    exhibitors := scrapeDetailsParallel(*headful, *timeout, links, *concurrency)
    log.Printf("Scraped %d exhibitors", len(exhibitors))

    if strings.HasSuffix(strings.ToLower(*out), ".csv") {
        if err := writeCSV(*out, exhibitors); err != nil {
            log.Fatalf("csv write: %v", err)
        }
    } else {
        if err := writeXLSX(*out, exhibitors); err != nil {
            log.Fatalf("xlsx write: %v", err)
        }
    }
    log.Println("Done!")
}

// ---------- Step 1: Render list page & collect detail links ----------
func renderListAndGetLinks(headful bool, timeout time.Duration) (string, []string, error) {
    ctx, cancel := newChromeCtx(headful, timeout)
    defer cancel()

    var html string
    var links []string

    linkJS := fmt.Sprintf(`(() => {
        const cards = document.querySelectorAll('%s');
        const out = [];
        cards.forEach(c => {
            let a = c.querySelector('%s');
            if (!a) return;
            let href = a.getAttribute('href') || '';
            if (!href && a.dataset && a.dataset.href) href = a.dataset.href;
            if (!href) {
                const oc = a.getAttribute('onclick') || '';
                const m = oc.match(/'(https?:[^']+)'/);
                if (m) href = m[1];
            }
            if (href) out.push(href);
        });
        return out;
    })()`, listSel.card, listSel.link)

    tasks := chromedp.Tasks{
        emulation.SetUserAgentOverride("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36"),
        chromedp.Navigate(startURL),
        chromedp.WaitVisible("body"),
        chromedp.Sleep(2 * time.Second),
        chromedp.ActionFunc(func(ctx context.Context) error { return autoLoadAll(ctx) }),
        chromedp.OuterHTML("html", &html),
        chromedp.Evaluate(linkJS, &links),
    }

    if err := chromedp.Run(ctx, tasks); err != nil {
        return "", nil, err
    }

    // Normalize links
    for i, l := range links {
        l = strings.TrimSpace(l)
        if strings.HasPrefix(l, "#") {
            l = startURL + l
        } else if strings.HasPrefix(l, "/") {
            l = "https://acma-automechanika-newdelhi.in.messefrankfurt.com" + l
        }
        links[i] = l
    }
    return html, links, nil
}

// Scroll/click until tile count stops growing
func autoLoadAll(ctx context.Context) error {
    var lastCount int
    for i := 0; i < 60; i++ {
        var count int
        if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`document.querySelectorAll('`+listSel.card+`').length`, &count)); err != nil {
            return err
        }
        // click load-more if present
        _ = chromedp.Click("button.load-more, .load-more button, .c-load-more button", chromedp.NodeVisible).Do(ctx)
        // scroll
        _ = chromedp.Evaluate(`window.scrollTo(0, document.body.scrollHeight)`, nil).Do(ctx)
        chromedp.Sleep(1200 * time.Millisecond).Do(ctx)
        if count == lastCount {
            break
        }
        lastCount = count
    }
    return nil
}

// ---------- Step 2: Scrape details in parallel ----------
func scrapeDetailsParallel(headful bool, timeout time.Duration, links []string, conc int) []Exhibitor {
    type job struct{ idx int; url string }
    type res struct{ idx int; ex Exhibitor; err error }

    jobs := make(chan job)
    results := make(chan res)

    var wg sync.WaitGroup
    for w := 0; w < conc; w++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            ctx, cancel := newChromeCtx(headful, timeout)
            defer cancel()
            for j := range jobs {
                ex, err := scrapeOne(ctx, j.url)
                if err != nil {
                    results <- res{j.idx, Exhibitor{}, err}
                } else {
                    ex.Link = j.url
                    results <- res{j.idx, ex, nil}
                }
            }
        }()
    }

    go func() {
        for i, u := range links {
            jobs <- job{i, u}
        }
        close(jobs)
        wg.Wait()
        close(results)
    }()

    out := make([]Exhibitor, len(links))
    for r := range results {
        if r.err != nil {
            log.Printf("[WARN] %s: %v", links[r.idx], r.err)
            continue
        }
        out[r.idx] = r.ex
    }

    // Filter empties
    var cleaned []Exhibitor
    for _, e := range out {
        if strings.TrimSpace(e.Name) != "" {
            cleaned = append(cleaned, e)
        }
    }
    return cleaned
}

// Create a new chrome ctx with timeout
func newChromeCtx(headful bool, timeout time.Duration) (context.Context, context.CancelFunc) {
    allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
        chromedp.Flag("headless", !headful),
        chromedp.Flag("disable-gpu", true),
        chromedp.Flag("no-sandbox", true),
    )
    allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
    ctx, cancelCtx := chromedp.NewContext(allocCtx)
    c, cancelTimeout := context.WithTimeout(ctx, timeout)
    return c, func() { cancelTimeout(); cancelCtx(); cancelAlloc() }
}

// Scrape one detail page
func scrapeOne(ctx context.Context, url string) (Exhibitor, error) {
    var body string
    tasks := chromedp.Tasks{
        chromedp.Navigate(url),
        chromedp.WaitVisible("body"),
        chromedp.Sleep(1500 * time.Millisecond), // give JS time
        chromedp.OuterHTML("html", &body),
    }
    if err := chromedp.Run(ctx, tasks); err != nil {
        return Exhibitor{}, err
    }
    return parseDetailHTML(body)
}

func parseDetailHTML(html string) (Exhibitor, error) {
    doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
    if err != nil {
        return Exhibitor{}, err
    }

    root := doc.Find(detailSel.root)
    if root.Length() == 0 {
        root = doc.Selection // fallback
    }

    ex := Exhibitor{
        Name:    textFirst(root.Find(detailSel.name)),
        Address: normalizeWhitespace(textFirst(root.Find(detailSel.address))),
        Phone:   firstNonEmpty(textFirst(root.Find(detailSel.phone)), findByRegex(root.Text(), phoneRx, 2)),
        Email:   firstNonEmpty(attrFirst(root.Find(detailSel.email), "href", "mailto:"), findByRegex(root.Text(), emailRx, 0)),
        Website: firstURL(root.Find(detailSel.website)),
    }
    ex = cleanExhibitor(ex)
    if ex.Name == "" {
        return ex, errors.New("name empty – selectors probably wrong")
    }
    return ex, nil
}

// ------------- Helpers -------------
func textFirst(s *goquery.Selection) string { return strings.TrimSpace(s.First().Text()) }

func attrFirst(s *goquery.Selection, attr, trimPrefix string) string {
    v, ok := s.First().Attr(attr)
    if !ok {
        return ""
    }
    return strings.TrimPrefix(strings.TrimSpace(v), trimPrefix)
}

func normalizeWhitespace(s string) string {
    return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func firstNonEmpty(vals ...string) string {
    for _, v := range vals {
        if strings.TrimSpace(v) != "" {
            return strings.TrimSpace(v)
        }
    }
    return ""
}

func findByRegex(text string, rx *regexp.Regexp, group int) string {
    rx.Longest()
    m := rx.FindStringSubmatch(strings.ReplaceAll(text, "\n", " "))
    if len(m) > group {
        return strings.TrimSpace(m[group])
    }
    return ""
}

func firstURL(sel *goquery.Selection) string {
    var url string
    sel.EachWithBreak(func(i int, s *goquery.Selection) bool {
        href, ok := s.Attr("href")
        if !ok {
            return true
        }
        if strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") {
            return true
        }
        if strings.HasPrefix(href, "http") {
            url = href
            return false
        }
        return true
    })
    return url
}

func cleanExhibitor(e Exhibitor) Exhibitor {
    strip := func(s string) string {
        s = strings.TrimSpace(strings.TrimPrefix(s, "Address:"))
        s = strings.TrimSpace(strings.TrimPrefix(s, "Tel:"))
        s = strings.TrimSpace(strings.TrimPrefix(s, "Phone:"))
        return s
    }
    e.Name = strip(e.Name)
    e.Address = strip(e.Address)
    e.Phone = strip(e.Phone)
    return e
}

func writeCSV(path string, rows []Exhibitor) error {
    f, err := os.Create(path)
    if err != nil {
        return err
    }
    defer f.Close()
    w := csv.NewWriter(f)
    defer w.Flush()
    _ = w.Write([]string{"Name", "Address", "Phone", "Email", "Website", "Source"})
    for _, r := range rows {
        if err := w.Write([]string{r.Name, r.Address, r.Phone, r.Email, r.Website, r.Link}); err != nil {
            return err
        }
    }
    return w.Error()
}

func writeXLSX(path string, rows []Exhibitor) error {
    f := excelize.NewFile()
    sheet := f.GetSheetName(0)
    headers := []string{"Name", "Address", "Phone", "Email", "Website", "Source"}
    for i, h := range headers {
        cell, _ := excelize.CoordinatesToCellName(i+1, 1)
        f.SetCellValue(sheet, cell, h)
    }
    for r, ex := range rows {
        vals := []string{ex.Name, ex.Address, ex.Phone, ex.Email, ex.Website, ex.Link}
        for c, v := range vals {
            cell, _ := excelize.CoordinatesToCellName(c+1, r+2)
            f.SetCellValue(sheet, cell, v)
        }
    }
    for i := 1; i <= len(headers); i++ {
        col, _ := excelize.ColumnNumberToName(i)
        _ = f.SetColWidth(sheet, col, col, 32)
    }
    return f.SaveAs(path)
}

func dedupe(in []string) []string {
    seen := make(map[string]struct{}, len(in))
    out := make([]string, 0, len(in))
    for _, v := range in {
        if _, ok := seen[v]; ok {
            continue
        }
        seen[v] = struct{}{}
        out = append(out, v)
    }
    return out
}
