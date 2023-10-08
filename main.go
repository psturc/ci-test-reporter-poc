package main

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/testgrid/metadata"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	v1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"sigs.k8s.io/yaml"

	reporters "github.com/onsi/ginkgo/v2/reporters"
	types "github.com/onsi/ginkgo/v2/types"

	"github.com/magefile/mage/sh"
)

const bucketName = "origin-ci-test"
const gcsBrowserURLPrefix = "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/origin-ci-test/"

const finishedFilename = "finished.json"
const buildLogFilename = "build-log.txt"
const junitFilename = "e2e-report.xml"

var bucketHandle *storage.BucketHandle

var artifactDir string

// listFiles lists objects within specified bucket.
func main() {

	rhtapJunitSuites := &reporters.JUnitTestSuites{}
	openshiftCiJunit := reporters.JUnitTestSuite{Name: "openshift-ci job", Properties: reporters.JUnitProperties{Properties: []reporters.JUnitProperty{}}}

	artifactDir = os.Getenv("ARTIFACT_DIR")
	if artifactDir == "" {
		artifactDir = "/tmp"
	}
	jobID := os.Getenv("PROW_JOB_ID")

	pjYAML, err := getProwJobYAML(jobID)
	if err != nil {
		log.Fatal(err)
	}

	jobTarget, err := determineJobTarget(pjYAML)
	if err != nil {
		log.Fatal(err)
	}

	pjURL := pjYAML.Status.URL

	sp := strings.Split(pjURL, bucketName)
	if len(sp) != 2 {
		log.Fatal("failed to determine object prefix from prow job url", pjURL)
	}
	objectPrefix := strings.TrimLeft(sp[1], "/")
	objectPrefix += "/artifacts/" + jobTarget

	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		fmt.Printf("storage.NewClient: %s", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()

	fmt.Println("object prefix", objectPrefix)
	bucketHandle = client.Bucket(bucketName)

	it := bucketHandle.Objects(ctx, &storage.Query{Prefix: objectPrefix})

	fmt.Println("tmp dir:", artifactDir)

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			panic(fmt.Sprintf("Bucket(%q).Objects: %s", bucketName, err))
		}
		if strings.Contains(attrs.Name, finishedFilename) || strings.Contains(attrs.Name, junitFilename) {
			sp := strings.Split(attrs.Name, objectPrefix)
			if len(sp) != 2 {
				log.Fatal("cannot determine filePath")
			}
			filePath := strings.TrimPrefix(sp[1], "/")
			fmt.Println("filepath: ", filePath)

			rc, err := bucketHandle.Object(attrs.Name).NewReader(ctx)
			if err != nil {
				log.Fatalf("Object(%q).NewReader: %s", attrs.Name, err)
			}
			if strings.Contains(attrs.Name, finishedFilename) {
				sp := strings.Split(filePath, "/")
				stepName := sp[0]

				if strings.Contains(attrs.Name, "gather") {
					openshiftCiJunit.Properties.Properties = append(openshiftCiJunit.Properties.Properties, reporters.JUnitProperty{Name: stepName, Value: gcsBrowserURLPrefix + strings.TrimSuffix(attrs.Name, finishedFilename) + "artifacts"})
				}
				data, err := io.ReadAll(rc)
				if err != nil {
					log.Fatalf("ioutil.ReadAll: %s", err)
				}

				finished := metadata.Finished{}
				err = yaml.Unmarshal(data, &finished)
				if err != nil {
					log.Fatal(err)
				}

				if *finished.Passed {
					fmt.Println(stepName, "has passed")
					openshiftCiJunit.TestCases = append(openshiftCiJunit.TestCases, reporters.JUnitTestCase{Name: stepName, Status: types.SpecStatePassed.String()})
				} else {
					buildLog := downloadBuildLog(strings.TrimSuffix(attrs.Name, finishedFilename))
					failure := &reporters.JUnitFailure{Message: fmt.Sprintf("%s has failed", stepName)}
					tc := reporters.JUnitTestCase{Name: stepName, Status: types.SpecStateFailed.String(), Failure: failure, SystemErr: buildLog}
					openshiftCiJunit.Failures++
					openshiftCiJunit.TestCases = append(openshiftCiJunit.TestCases, tc)
				}
				openshiftCiJunit.Tests++

				rc.Close()
				continue
			}
			// decode rhtap suites
			rhtapJunitSuites.Failures += openshiftCiJunit.Failures
			rhtapJunitSuites.Errors += openshiftCiJunit.Errors
			rhtapJunitSuites.Tests += openshiftCiJunit.Tests
			if err = xml.NewDecoder(rc).Decode(rhtapJunitSuites); err != nil {
				log.Fatal(err)
			}

		}
	}
	localFilePath := artifactDir + "/junit.xml"

	rhtapJunitSuites.TestSuites = append(rhtapJunitSuites.TestSuites, openshiftCiJunit)

	outFile, err := os.Create(localFilePath)
	if err != nil {
		log.Fatal(err)
	}

	if err := xml.NewEncoder(bufio.NewWriter(outFile)).Encode(rhtapJunitSuites); err != nil {
		log.Fatal(err)
	}
	fmt.Println(localFilePath)

	if err := sh.RunV("go", "install", "-mod=mod", "github.com/psturc/junit2html@experiment"); err != nil {
		log.Fatal(err)
	}
	if err := sh.RunV("bash", "-c", fmt.Sprintf("junit2html < %s/junit.xml > %s/junit-summary.html", artifactDir, artifactDir)); err != nil {
		log.Fatal(err)
	}
	// if err := os.WriteFile(localFilePath, data, 0755); err != nil {
	// 	log.Fatalf("can't write data to a file %s: %+v", localFilePath, err)
	// }
}

func downloadBuildLog(pathToStep string) string {
	r, err := bucketHandle.Object(pathToStep + buildLogFilename).NewReader(context.TODO())
	if err != nil {
		log.Fatal(err)
	}
	c, err := io.ReadAll(r)
	if err != nil {
		log.Fatal(err)
	}
	return string(c)
}

func getProwJobYAML(jobID string) (*v1.ProwJob, error) {
	r, err := http.Get(fmt.Sprintf("https://prow.ci.openshift.org/prowjob?prowjob=%s", jobID))
	errTemplate := "failed to get prow job YAML:"
	if err != nil {
		return nil, fmt.Errorf("%s %s", errTemplate, err)
	}
	if r.StatusCode > 299 {
		return nil, fmt.Errorf("%s got response status code %v", errTemplate, r.StatusCode)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s", errTemplate, err)
	}
	var pj v1.ProwJob
	err = yaml.Unmarshal(body, &pj)
	if err != nil {
		return nil, fmt.Errorf("%s %s", errTemplate, err)
	}
	return &pj, nil
}

func determineJobTarget(pjYAML *v1.ProwJob) (jobTarget string, err error) {
	for _, arg := range pjYAML.Spec.PodSpec.Containers[0].Args {
		if strings.Contains(arg, "--target") {
			sp := strings.Split(arg, "=")
			if len(sp) != 2 {
				log.Fatal("failed to determine job target")
			}
			jobTarget = sp[1]
			return
		}
	}
	return "", fmt.Errorf("failed to determine job target")
}
