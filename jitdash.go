package main

import (
    "bytes"
    "encoding/json"
    "errors"
    "fmt"
    "log"
    "net/http"
    "os"
    "regexp"
    "sort"
    "time"
)

type JsonObject map[string]interface{}

func AsJsonObject(i interface{}) (JsonObject, bool) {
    if o, ok := i.(map[string]interface{}); ok {
        return JsonObject(o), true
    }
    return nil, false
}

func (o JsonObject) GetString(key string) (string, bool) {
    val, ok := o[key]
    if !ok {
        return "", false
    }

    strVal, ok := val.(string)
    return strVal, ok
}

func (o JsonObject) GetInt64(key string) (int64, bool) {
    val, ok := o[key]
    if !ok {
        return 0, false
    }

    switch v := val.(type) {
    case json.Number:
        i64, err := v.Int64()
        if err != nil {
            return 0, false
        }
        return i64, true

    case float64:
        return int64(v), true
    }

    return 0, false
}

func (o JsonObject) GetBool(key string) (bool, bool) {
    val, ok := o[key]
    if !ok {
        return false, false
    }

    bVal, ok := val.(bool)
    return bVal, ok
}

func (o JsonObject) GetObject(key string) (JsonObject, bool) {
    val, ok := o[key]
    if !ok {
        return nil, false
    }

    oVal, ok := AsJsonObject(val)
    return oVal, ok
}

func (o JsonObject) GetArray(key string) ([]interface{}, bool) {
    val, ok := o[key]
    if !ok {
        return nil, false
    }

    aVal, ok := val.([]interface{})
    return aVal, ok
}

type Instance struct {
    Name string
    Folders []string // list of folder URLs of the form "/abs/path/to/job/"
    Exclude []*regexp.Regexp // list of REs for jobs to exclude
}

type Build struct {
    Id int64
    Url string
    Timestamp time.Time
    Failures int64
    Complete bool
}

type Job struct {
    Name string
    Url string
    Builds []*Build
}

func (i* Instance) ProcessBuildObject(buildIf interface{}) (*Build, bool) {
    build, ok := AsJsonObject(buildIf)
    if !ok {
        return nil, false
    }

    if class, ok := build.GetString("_class"); !ok || class != "hudson.model.FreeStyleBuild" {
        return nil, false
    }

    id, ok := build.GetInt64("number")
    if !ok {
        return nil, false
    }

    url, ok := build.GetString("url")
    if !ok {
        return nil, false
    }

    return &Build{Id: id, Url: url}, true
}

var missingResultError = errors.New("missing result")
var missingTimestampError = errors.New("missing timestamp")
func (b *Build) FetchDetails() error {
    r, err := http.Get(b.Url + "api/json")
    if err != nil {
        return err
    }

    var details JsonObject
    if err = json.NewDecoder(r.Body).Decode(&details); err != nil {
        r.Body.Close()
        return err
    }
    r.Body.Close()

    result, ok := details.GetString("result")
    if !ok {
        return missingResultError
    }

    unixMilliseconds, ok := details.GetInt64("timestamp")
    if !ok {
        return missingTimestampError
    }
    b.Timestamp = time.Unix(unixMilliseconds / 1000, 0).UTC()

    building, ok := details.GetBool("building")
    if !ok {
        building = result == ""
    }
    b.Complete = !building

    var failures int64
    if actions, ok := details.GetArray("actions"); ok {
        for _, a := range actions {
            action, ok := AsJsonObject(a)
            if !ok {
                continue
            }

            if class, ok := action.GetString("_class"); !ok || class != "hudson.tasks.junit.TestResultAction" {
                continue
            }

            failures, _ = action.GetInt64("failCount")
        }
    }

    if failures == 0 && result == "FAILURE" {
        failures = -1
    }

    b.Failures = failures
    return nil
}

type BuildSorter []*Build

func (s BuildSorter) Len() int {
    return len(s)
}

func (s BuildSorter) Swap(i, j int) {
    s[i], s[j] = s[j], s[i]
}

func (s BuildSorter) Less(i, j int) bool {
    return s[i].Id < s[j].Id
}

func (i *Instance) ProcessJobObject(jobIf interface{}) (*Job, bool) {
    job, ok := AsJsonObject(jobIf)
    if !ok {
        return nil, false
    }

    if class, ok := job.GetString("_class"); !ok || class != "hudson.model.FreeStyleProject" {
        return nil, false
    }

    name, ok := job.GetString("name")
    if !ok {
        return nil, false
    }

    for _, ex := range i.Exclude {
        if ex.MatchString(name) {
            log.Printf("excluded job %s\n", name)
            return nil, false
        }
    }

    url, ok := job.GetString("url")
    if !ok {
        return nil, false
    }

    r, err := http.Get(url + "api/json")
    if err != nil {
        return nil, false
    }

    var details JsonObject
    if err = json.NewDecoder(r.Body).Decode(&details); err != nil {
        r.Body.Close()
        return nil, false
    }
    r.Body.Close()

    buildObjects, ok := details.GetArray("builds")
    if !ok {
        return nil, false
    }

    log.Printf("processing builds for job %s\n", name)

    var builds []*Build
    for _, b := range buildObjects {
        build, ok := i.ProcessBuildObject(b)
        if ok {
            builds = append(builds, build)
        }
    }

    sort.Sort(BuildSorter(builds))
    return &Job{name, url, builds}, true
}

func (i *Instance) FetchJobs() []*Job {
    log.Printf("fetching jobs for instance %s\n", i.Name)

    var jobs []*Job
    for _, folderUrl := range i.Folders {
        log.Printf("fetching folder %s\n", folderUrl)

        r, err := http.Get(folderUrl)
        if err != nil {
            log.Printf("error fetching folder %s: %s\n", folderUrl, err)
            continue
        }

        var folder JsonObject
        if err = json.NewDecoder(r.Body).Decode(&folder); err != nil {
            log.Printf("error reading folder %s: %s\n", folderUrl, err)
            r.Body.Close()
            continue
        }
        r.Body.Close()

        jobObjects, ok := folder.GetArray("jobs")
        if !ok {
            continue
        }

        for _, j := range jobObjects {
            job, ok := i.ProcessJobObject(j)
            if ok {
                jobs = append(jobs, job)
            }
        }
    }

    return jobs
}

var sparks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
func (job *Job) RenderHistory(count int) string {
    log.Printf("Rendering job %s\n", job.Name)

    w := new(bytes.Buffer)
    printf := func(format string, a ...interface{}) {
        fmt.Fprintf(w, format, a...)
    }

    for ; count > len(job.Builds); count-- {
        printf("%c", sparks[0])
    }

    start := len(job.Builds) - count

    max := int64(0)
    for i := start; i < len(job.Builds); i++ {
        if f := job.Builds[i].Failures; f > max {
            max = f
        }
    }

    for i := start; i < len(job.Builds); i++ {
        build := job.Builds[i]

        var spark rune
        var title string
        if build.Complete {
            switch f := build.Failures; f {
            case 0:
                spark = sparks[0]
                title = "Passed"
                break

            case -1:
                spark = sparks[len(sparks) - 1]
                title = "Failed"
                break

            default:
                percentile := float64(f) / float64(max)
                spark = sparks[1 + int(percentile * float64(len(sparks) - 2))]
                title = fmt.Sprintf("%d failures", f)
            }
        } else {
            spark = 'B'
            title = "building"
        }

        printf("<a href=\"%s\" title=\"%s: %s\">%c</a>", build.Url, title, build.Url, spark)
    }

    return w.String()
}

func ProcessInstanceObject(instanceIf interface{}, name string) (*Instance, error) {
    instanceObject, ok := AsJsonObject(instanceIf)
    if !ok {
        return nil, errors.New(fmt.Sprintf("Instance %s is not an object", name))
    }

    foldersArray, ok := instanceObject.GetArray("folders")
    if !ok {
        return nil, errors.New(fmt.Sprintf("Instance %s specifies no folders", name))
    }

    var folders []string
    for _, f := range foldersArray {
        folder, ok := f.(string)
        if !ok {
            return nil, errors.New(fmt.Sprintf("Instance %s contains an invalid folder: %s", name, f))
        }
        folders = append(folders, folder + "api/json")
    }

    var exclude []*regexp.Regexp
    excludeArray, ok := instanceObject.GetArray("exclude")
    if ok {
        for _, e := range excludeArray {
            estr, ok := e.(string)
            if !ok {
                return nil, errors.New(fmt.Sprintf("Instance %s contains an invalid exclude: %s", name, e))
            }

            ex, err := regexp.Compile(estr)
            if err != nil {
                return nil, errors.New(fmt.Sprintf("Instance %s contains an invalid exclude %s: %s", name, estr, err))
            }

            exclude = append(exclude, ex)
        }
    }

    return &Instance{name, folders, exclude}, nil
}

func main() {
    var config JsonObject
    if err := json.NewDecoder(os.Stdin).Decode(&config); err != nil {
        fmt.Fprintf(os.Stderr, "could not read config: %s\n", err)
        os.Exit(-1)
    }

    maxBuilds, ok := config.GetInt64("maxBuilds")
    if !ok {
        maxBuilds = 10
    }

    maxHistory, ok := config.GetInt64("maxHistory")
    if !ok {
        maxHistory = 10
    }

    if maxHistory > maxBuilds {
        maxHistory = maxBuilds
    }

    instancesObject, ok := config.GetObject("instances")
    if !ok {
        fmt.Fprintf(os.Stderr, "invalid config: no instances\n")
        os.Exit(-1)
    }

    var instances []*Instance
    for k, v := range instancesObject {
        i, err := ProcessInstanceObject(v, k)
        if err != nil {
            fmt.Fprintf(os.Stderr, "invalid config: %s\n", err)
            os.Exit(-1)
        }
        instances = append(instances, i)
    }

    var jobs [][]*Job
    for _, i := range instances {
        jobs = append(jobs, i.FetchJobs())
    }

    // Fetch build details in parallel
    const workerCount = 100
    work, done := make(chan *Build, workerCount), make(chan bool, workerCount)
    for i := 0; i < workerCount; i++ {
        go func(w <-chan *Build, d chan<- bool) {
            for b := range w {
                b.FetchDetails()
            }
            done <- true
        }(work, done)
    }

    log.Print("Fetching build details...\n")
    for _, ja := range jobs {
        for _, j := range ja {
            if len(j.Builds) > int(maxBuilds) {
                j.Builds = j.Builds[len(j.Builds) - int(maxBuilds):]
            }
            for _, b := range j.Builds {
                work <- b
            }
        }
    }
    close(work)

    for i := 0; i < workerCount; i++ {
        <-done
    }
    close(done)

    fmt.Printf("<html><head><style>td.sparkline { font-family: \"Consolas, \\\"Liberation Mono\\\", Menlo, Courier, monospace\"; font-size: 12px }</style></head><body>\n")
    for n, i := range instances {
        fmt.Printf("<h2>%s</h2>\n", i.Name)
        fmt.Printf("<table><tr><th>Job</th><th>History</th></tr>\n")
        for _, job := range jobs[n] {
            fmt.Printf("<tr><td><a href=\"%s\">%s</a></td><td class=\"sparkline\">%s</td></tr>\n", job.Url, job.Name, job.RenderHistory(int(maxHistory)))
        }
        fmt.Printf("</table><br />\n")
    }
    fmt.Printf("</body></html>\n")
}
