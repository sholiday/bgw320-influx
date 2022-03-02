package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/avast/retry-go"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	influxApi "github.com/influxdata/influxdb-client-go/v2/api"
	influxWrite "github.com/influxdata/influxdb-client-go/v2/api/write"
	"github.com/kelseyhightower/envconfig"
)

type Specification struct {
	RouterIp   string
	InfluxHost string
	InfluxPort int
	InfluxUser string
	InfluxPass string
}

type App struct {
	influxWriteApi influxApi.WriteAPIBlocking
	s              Specification
}

func NewApp(s Specification) (*App, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time.Second*60))
	defer cancel()

	authString := fmt.Sprintf("%s:%s", s.InfluxUser, s.InfluxPass)
	hostPort := fmt.Sprintf("http://%s:%d", s.InfluxHost, s.InfluxPort)
	influxC := influxdb2.NewClient(hostPort, authString)
	health, err := influxC.Health(ctx)
	if err != nil {
		return nil, err
	}
	if health.Status != "pass" {
		return nil, fmt.Errorf("Could not work with influx db")
	}

	influxWriteApi := influxC.WriteAPIBlocking("", "db0")
	_ = influxWriteApi

	return &App{
		influxWriteApi: influxWriteApi,
		s:              s,
	}, nil
}

func (a *App) retrieve(ctx context.Context) (*goquery.Document, error) {
	urlA := fmt.Sprintf("http://%s/cgi-bin/broadbandstatistics.ha", a.s.RouterIp)
	resp, err := http.Get(urlA)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return goquery.NewDocumentFromReader(resp.Body)
}

func (a *App) parseData(doc *goquery.Document, t time.Time) ([]*influxWrite.Point, error) {
	var points []*influxWrite.Point
	doc.Find("table").Each(func(i int, table *goquery.Selection) {
		fields := map[string]interface{}{}
		sum, has := table.Attr("summary")
		if !has || sum != "Ethernet IPv4 Statistics Table" {
			return
		}
		table.Find("tr").Each(func(i int, row *goquery.Selection) {
			var name string
			var value string
			row.Find("th").First().Each(func(i int, th *goquery.Selection) {
				name = th.Text()
			})
			row.Find("td").First().Each(func(i int, td *goquery.Selection) {
				value = td.Text()
			})
			name = strings.ToLower(strings.Replace(strings.TrimSpace(name), " ", "_", -1))
			value = strings.TrimSpace(value)
			valInt, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				log.Printf("Couldn't parse '%s' for '%s'\n", value, name)
			}
			fields[name] = valInt
		})
		log.Println(fields)

		points = append(points, influxdb2.NewPoint(
			"ethernet_ipv4",
			map[string]string{
				"router": a.s.RouterIp,
			},
			fields,
			t))

	})
	return points, nil
}

func (a *App) Run() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time.Second*60))
	defer cancel()

	var doc *goquery.Document
	err := retry.Do(
		func() error {
			var err error
			doc, err = a.retrieve(ctx)
			return err
		},
		retry.Context(ctx),
	)
	if err != nil {
		return err
	}
	t := time.Now()
	points, err := a.parseData(doc, t)
	if err != nil {
		return err
	}

	err = a.influxWriteApi.WritePoint(ctx, points...)
	if err != nil {
		return err
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	var s Specification
	err := envconfig.Process("BGW320", &s)
	if err != nil {
		log.Fatal(err.Error())
	}
	log.Printf("%+v", s)

	app, err := NewApp(s)
	if err != nil {
		log.Fatal(err)
	}

	err = app.Run()
	if err != nil {
		log.Fatal(err)
	}
	for {
		select {
		case <-time.Tick(5 * time.Second):
			err := app.Run()
			if err != nil {
				log.Printf("Failed to submit data: %s", err)
			}
		}
	}

}
