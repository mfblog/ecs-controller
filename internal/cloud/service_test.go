package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

func TestServicePreflightAndEIPRequestSemantics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("Action")
		var response any
		switch action {
		case "DescribeInstanceTypes":
			response = map[string]any{"InstanceTypes": map[string]any{"InstanceType": []any{map[string]any{"InstanceTypeId": "ecs.test", "CpuArchitecture": "X86"}}}}
		case "DescribeAvailableResource":
			if r.URL.Query().Get("DestinationResource") == "SystemDisk" {
				response = map[string]any{"AvailableResources": map[string]any{"AvailableResource": []any{map[string]any{"Value": "cloud_essd", "MinSystemDiskSize": 40, "MaxSystemDiskSize": 200}}}}
			} else {
				response = map[string]any{"AvailableZones": map[string]any{"AvailableZone": []any{map[string]any{"ZoneId": "zone-a", "Status": "Available"}}}}
			}
		case "DescribeImages":
			response = map[string]any{"Images": map[string]any{"Image": []any{map[string]any{"ImageId": "img-1", "OSName": "Ubuntu 22.04", "Architecture": "x86_64"}}}}
		case "RunInstances":
			if r.URL.Query().Get("AllocatePublicIp") != "false" || r.URL.Query().Get("InternetMaxBandwidthOut") != "0" || r.URL.Query().Get("ClientToken") != "task-1" {
				t.Fatalf("unexpected RunInstances parameters: %v", r.URL.Query())
			}
			response = map[string]any{"InstanceId": "i-1"}
		case "AllocateEipAddress":
			if r.URL.Query().Get("Bandwidth") != "20" {
				t.Fatalf("unexpected EIP bandwidth: %v", r.URL.Query())
			}
			response = map[string]any{"AllocationId": "eip-1", "EipAddress": "203.0.113.10"}
		default:
			response = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2014-05-26", Product: "Ecs", AccessKey: "ak", Secret: "sk"}
	service := &Service{ECS: client, EIP: client}
	typeInfo, err := service.DescribeInstanceType(context.Background(), "cn-test", "ecs.test")
	if err != nil || typeInfo["CpuArchitecture"] != "X86" {
		t.Fatalf("instance type: %#v %v", typeInfo, err)
	}
	zones, err := service.DescribeAvailableZones(context.Background(), "cn-test", "ecs.test", "cloud_essd")
	if err != nil || len(zones) != 1 || zones[0]["ZoneId"] != "zone-a" {
		t.Fatalf("zones: %#v %v", zones, err)
	}
	images, err := service.DescribeImagesForArchitecture(context.Background(), "cn-test", "ubuntu_22", "x86_64")
	if err != nil || len(images) != 1 {
		t.Fatalf("images: %#v %v", images, err)
	}
	disks, err := service.GetSystemDiskOptions(context.Background(), "cn-test", "zone-a", "ecs.test")
	if err != nil || len(disks) != 1 || disks[0]["min"] != 40 {
		t.Fatalf("disks: %#v %v", disks, err)
	}
	result, err := service.RunInstances(context.Background(), RunRequest{RegionID: "cn-test", ZoneID: "zone-a", InstanceType: "ecs.test", ImageID: "img-1", InstanceName: "test", SecurityGroupID: "sg-1", VSwitchID: "vs-1", Bandwidth: 20, PublicIPMode: "eip", Password: "Password123!", ClientToken: "task-1"})
	if err != nil || result.InstanceID != "i-1" {
		t.Fatalf("run result: %#v %v", result, err)
	}
	allocationID, ip, err := service.AllocateEIPWithBandwidth(context.Background(), "cn-test", 20)
	if err != nil || allocationID != "eip-1" || ip != "203.0.113.10" {
		t.Fatalf("allocate EIP result: %q %q %v", allocationID, ip, err)
	}
}

func TestDescribeInstancesPaginatesAndRejectsIncompletePages(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("PageNumber")
		if r.URL.Query().Get("PageSize") != "100" {
			t.Fatalf("unexpected page size: %s", r.URL.Query().Get("PageSize"))
		}
		items := make([]map[string]any, 0)
		if page == "1" {
			for i := 0; i < 100; i++ {
				items = append(items, map[string]any{"InstanceId": fmt.Sprintf("i-%d", i), "Status": "Running"})
			}
		} else if page == "2" {
			items = append(items, map[string]any{"InstanceId": "i-100", "Status": "Stopped"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"TotalCount": 101, "Instances": map[string]any{"Instance": items}})
	}))
	defer server.Close()

	service := &Service{ECS: &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2014-05-26", Product: "Ecs", AccessKey: "ak", Secret: "sk"}}
	instances, err := service.DescribeInstances(context.Background(), "cn-test")
	if err != nil || len(instances) != 101 || calls != 2 || instances[100].ID != "i-100" {
		t.Fatalf("pagination result: count=%d calls=%d err=%v", len(instances), calls, err)
	}

	emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"TotalCount":1,"Instances":{"Instance":[]}}`))
	}))
	defer emptyServer.Close()
	emptyService := &Service{ECS: &RPCClient{HTTPClient: emptyServer.Client(), Endpoint: emptyServer.URL, Version: "2014-05-26", Product: "Ecs", AccessKey: "ak", Secret: "sk"}}
	if _, err := emptyService.DescribeInstances(context.Background(), "cn-test"); err == nil {
		t.Fatal("incomplete instance page was accepted")
	}
}

func TestDiskOptionsResponseNestedSupportedResources(t *testing.T) {
	root := map[string]any{"AvailableZones": map[string]any{"AvailableZone": []any{map[string]any{"AvailableResources": map[string]any{"AvailableResource": []any{map[string]any{"SupportedResources": map[string]any{"SupportedResource": []any{map[string]any{"Value": "cloud_efficiency"}}}}}}}}}}
	options := diskOptionsFromResponse(root)
	if len(options) != 1 || options[0]["value"] != "cloud_efficiency" {
		t.Fatalf("options: %#v", options)
	}
}

func TestDiskCategoryLabelsNeverRenderBlank(t *testing.T) {
	for _, category := range []string{"cloud_essd_entry", "cloud_essd", "cloud_auto", "cloud_unknown"} {
		if label := diskCategoryLabel(category); label == "" {
			t.Fatalf("disk category %q has an empty label", category)
		}
	}
	if diskCategoryLabel("cloud_auto") != "ESSD AutoPL" {
		t.Fatalf("unexpected ESSD AutoPL label: %q", diskCategoryLabel("cloud_auto"))
	}
}

func TestOutboundTrafficUsesCMSDimensionObject(t *testing.T) {
	var dimensions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dimensions = append(dimensions, r.URL.Query().Get("Dimensions"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"200","Datapoints":"[]"}`))
	}))
	defer server.Close()

	service := &Service{CMS: &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2019-01-01", Product: "Cms", AccessKey: "ak", Secret: "sk"}}
	_, _, _, _, err := service.GetOutboundTrafficDelta(context.Background(), "cn-hongkong", "i-1", "203.0.113.10", 1000, 2000)
	if err == nil {
		t.Fatal("empty CMS datapoints were accepted as a zero-traffic sample")
	}
	if !IsMetricNoDataError(err) {
		t.Fatalf("empty CMS datapoints were not classified as delayed cloud data: %v", err)
	}
	if len(dimensions) != 2 || dimensions[0] != `{"instanceId":"i-1","ip":"203.0.113.10"}` || dimensions[1] != `{"instanceId":"i-1"}` {
		t.Fatalf("unexpected CMS dimensions: %#v", dimensions)
	}
}

func TestMetricNoDataErrorDoesNotMatchRegularAPIError(t *testing.T) {
	if IsMetricNoDataError(fmt.Errorf("aliyun Throttling.User: request was denied")) {
		t.Fatal("regular API errors must not be classified as missing metric data")
	}
}

func TestMonthlyTrafficUsesHourlyCMSData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Period") != "3600" {
			t.Fatalf("monthly query used the wrong period: %s", r.URL.Query().Get("Period"))
		}
		if r.URL.Query().Get("StartTime") != "1000" || r.URL.Query().Get("EndTime") != "5000" {
			t.Fatalf("monthly query used the wrong range: %s - %s", r.URL.Query().Get("StartTime"), r.URL.Query().Get("EndTime"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"200","Datapoints":"[{\"timestamp\":2000,\"Average\":8}]"}`))
	}))
	defer server.Close()

	service := &Service{CMS: &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2019-01-01", Product: "Cms", AccessKey: "ak", Secret: "sk"}}
	bytes, points, err := service.GetInstanceMonthlyTraffic(context.Background(), "cn-hongkong", "i-1", "203.0.113.10", 1000, 5000)
	if err != nil || points != 1 || bytes != 3600 {
		t.Fatalf("monthly traffic: bytes=%v points=%d err=%v", bytes, points, err)
	}
}

func TestDailyTrafficUsesExactCalendarWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Period") != "3600" {
			t.Fatalf("daily query used the wrong period: %s", r.URL.Query().Get("Period"))
		}
		if r.URL.Query().Get("Length") != "48" {
			t.Fatalf("daily query used the wrong length: %s", r.URL.Query().Get("Length"))
		}
		if r.URL.Query().Get("StartTime") != "1753920000000" || r.URL.Query().Get("EndTime") != "1754006400000" {
			t.Fatalf("daily query used the wrong range: %s - %s", r.URL.Query().Get("StartTime"), r.URL.Query().Get("EndTime"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"Code\":\"200\",\"Datapoints\":\"[{\\\"timestamp\\\":1753923600000,\\\"Average\\\":8}]\"}"))
	}))
	defer server.Close()

	service := &Service{CMS: &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2019-01-01", Product: "Cms", AccessKey: "ak", Secret: "sk"}}
	bytes, points, err := service.GetInstanceDailyTraffic(context.Background(), "cn-hongkong", "i-1", "203.0.113.10", 1753920000000, 1754006400000)
	if err != nil || points != 1 || bytes != 3600 {
		t.Fatalf("daily traffic: bytes=%v points=%d err=%v", bytes, points, err)
	}
}

func TestInstanceFromMapSupportsCurrentECSFieldsAndNestedIPs(t *testing.T) {
	instance := instanceFromMap(map[string]any{
		"InstanceId":     "i-1",
		"InstanceName":   "demo",
		"Status":         "Running",
		"InstanceTypeId": "ecs.test",
		"CpuCoreCount":   "4",
		"MemorySizeInMB": "8192",
		"OSNameEn":       "Ubuntu 22.04",
		"EipAddress":     map[string]any{"IpAddress": []any{"203.0.113.10"}},
		"VpcAttributes":  map[string]any{"PrivateIpAddress": map[string]any{"IpAddress": []any{"172.16.0.10"}}},
	})
	if instance.InstanceType != "ecs.test" || instance.CPU != 4 || instance.Memory != 8192 || instance.OSName != "Ubuntu 22.04" {
		t.Fatalf("instance hardware fields were not parsed: %#v", instance)
	}
	if instance.PublicIP != "203.0.113.10" || instance.PrivateIP != "172.16.0.10" {
		t.Fatalf("nested IP fields were not parsed: %#v", instance)
	}
}

func TestHongKongCountsAsOverseasCDTRegion(t *testing.T) {
	if !overseasRegion("cn-hongkong") || overseasRegion("cn-shanghai") || !overseasRegion("ap-southeast-1") {
		t.Fatal("CDT region classification is incorrect")
	}
}

func TestGetBillingDetailsFallsBackToDailyBillingItemsAndPaginates(t *testing.T) {
	var mu sync.Mutex
	requests := make([]url.Values, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		requests = append(requests, query)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if query.Get("Action") == "DescribeSplitItemBill" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Success", "Data": map[string]any{"Items": []map[string]any{}}})
			return
		}
		item := map[string]any{
			"BillingDate":      "2026-08-01",
			"ProductName":      "云服务器 ECS",
			"ProductCode":      "ecs",
			"ProductDetail":    "按量付费 ECS 计算资源",
			"BillingItem":      "计算资源",
			"BillingItemCode":  "instance",
			"BillingType":      "按量付费",
			"SubscriptionType": "PayAsYouGo",
			"PretaxAmount":     "1.25",
			"Currency":         "CNY",
			"InstanceID":       "i-1",
			"Usage":            "24",
			"UsageUnit":        "小时",
		}
		data := map[string]any{"Items": []map[string]any{item}}
		if query.Get("NextToken") == "" {
			data["NextToken"] = "next-page"
		} else {
			item["BillingItem"] = "云盘"
			item["BillingItemCode"] = "disk"
			item["PretaxAmount"] = "0.25"
		}
		response := map[string]any{"Code": "Success", "Data": data}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	service := &Service{BSS: &RPCClient{
		Endpoint:   server.URL,
		Version:    "2017-12-14",
		Product:    "BssOpenApi",
		AccessKey:  "ak",
		Secret:     "secret",
		HTTPClient: server.Client(),
	}}
	details, err := service.GetBillingDetails(context.Background(), "china", "2026-08", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 2 || details[0].Date != "2026-08-01" || details[0].ProductDetail != "按量付费 ECS 计算资源" {
		t.Fatalf("unexpected billing details: %#v", details)
	}
	itemsByCode := map[string]BillingDetail{}
	for _, detail := range details {
		itemsByCode[detail.BillingItemCode] = detail
	}
	if item := itemsByCode["instance"]; item.Amount != 1.25 || item.Usage != 24 || item.Unit != "小时" {
		t.Fatalf("instance billing item was not parsed: %#v", item)
	}
	if item := itemsByCode["disk"]; item.Amount != 0.25 || item.BillingItem != "云盘" {
		t.Fatalf("disk billing item was not parsed: %#v", item)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("billing API calls=%d, want 3", len(requests))
	}
	if got := requests[0].Get("Action"); got != "DescribeSplitItemBill" {
		t.Fatalf("split bill action=%q, want DescribeSplitItemBill", got)
	}
	if got := requests[0].Get("IsBillingItem"); got != "" {
		t.Fatalf("split bill unexpectedly received IsBillingItem=%q", got)
	}
	for page, request := range requests[1:] {
		if got := request.Get("Action"); got != "DescribeInstanceBill" {
			t.Fatalf("page %d action=%q, want DescribeInstanceBill", page+1, got)
		}
		if got := request.Get("Granularity"); got != "DAILY" {
			t.Fatalf("page %d granularity=%q, want DAILY", page+1, got)
		}
		if got := request.Get("BillingCycle"); got != "2026-08" {
			t.Fatalf("page %d billing cycle=%q", page+1, got)
		}
		if got := request.Get("BillingDate"); got != "2026-08-01" {
			t.Fatalf("page %d billing date=%q", page+1, got)
		}
		if got := request.Get("IsBillingItem"); got != "true" {
			t.Fatalf("page %d billing item flag=%q", page+1, got)
		}
		if got := request.Get("IsHideZeroCharge"); got != "true" {
			t.Fatalf("page %d hide zero charge=%q", page+1, got)
		}
		if got := request.Get("MaxResults"); got != "300" {
			t.Fatalf("page %d max results=%q", page+1, got)
		}
	}
	if got := requests[1].Get("NextToken"); got != "" {
		t.Fatalf("first page next token=%q", got)
	}
	if got := requests[2].Get("NextToken"); got != "next-page" {
		t.Fatalf("second page next token=%q", got)
	}
}

func TestGetBillingDetailsUsesSplitBillWhenAvailable(t *testing.T) {
	var mu sync.Mutex
	actions := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("Action")
		mu.Lock()
		actions = append(actions, action)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		data := map[string]any{"Items": []map[string]any{{
			"BillingDate":     "2026-08-01",
			"ProductName":     "对象存储 OSS",
			"BillingItem":     "存储容量",
			"BillingItemCode": "storage",
			"PretaxAmount":    "0.30",
			"Currency":        "CNY",
		}}}
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Success", "Data": data})
	}))
	defer server.Close()

	service := &Service{BSS: &RPCClient{
		Endpoint:   server.URL,
		Version:    "2017-12-14",
		Product:    "BssOpenApi",
		AccessKey:  "ak",
		Secret:     "secret",
		HTTPClient: server.Client(),
	}}
	details, err := service.GetBillingDetails(context.Background(), "china", "2026-08", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 1 || details[0].ProductName != "对象存储 OSS" || details[0].Amount != 0.30 {
		t.Fatalf("unexpected split fallback details: %#v", details)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 1 || actions[0] != "DescribeSplitItemBill" {
		t.Fatalf("unexpected primary actions: %#v", actions)
	}
}

func TestGetBillingDetailsUsesInstanceBillWhenSplitBillIsUnavailable(t *testing.T) {
	var mu sync.Mutex
	actions := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("Action")
		mu.Lock()
		actions = append(actions, action)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if action == "DescribeSplitItemBill" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OperationDenied", "Message": "split bill unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Success", "Data": map[string]any{"Items": []map[string]any{{
			"BillingDate":     "2026-08-01",
			"ProductName":     "云服务器 ECS",
			"BillingItem":     "计算资源",
			"BillingItemCode": "instance",
			"PretaxAmount":    "0.42",
			"Currency":        "CNY",
		}}}})
	}))
	defer server.Close()

	service := &Service{BSS: &RPCClient{
		Endpoint:   server.URL,
		Version:    "2017-12-14",
		Product:    "BssOpenApi",
		AccessKey:  "ak",
		Secret:     "secret",
		HTTPClient: server.Client(),
	}}
	details, err := service.GetBillingDetails(context.Background(), "china", "2026-08", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 1 || details[0].BillingItemCode != "instance" || details[0].Amount != 0.42 {
		t.Fatalf("unexpected instance fallback details: %#v", details)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 2 || actions[0] != "DescribeSplitItemBill" || actions[1] != "DescribeInstanceBill" {
		t.Fatalf("unexpected fallback actions: %#v", actions)
	}
}

func TestBillingDetailPreservesConfigurationAndServicePeriod(t *testing.T) {
	detail := billingDetailFromMap(map[string]any{
		"BillingDate":       "2026-08-02",
		"ProductName":       "云服务器 ECS",
		"BillingItem":       "系统盘大小",
		"Usage":             "50",
		"UsageUnit":         "GiB",
		"InstanceConfig":    "系统盘：2 GiB",
		"ServicePeriod":     "86400",
		"ServicePeriodUnit": "秒",
	}, "", "CNY", "2026-08-02")

	if detail.InstanceConfig != "系统盘：2 GiB" || detail.ServicePeriod != 86400 || detail.ServicePeriodUnit != "秒" {
		t.Fatalf("billing metadata was lost: %#v", detail)
	}
}

func TestDescribeBillingResourcesMapsSystemDiskAndEIP(t *testing.T) {
	requests := make([]url.Values, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		requests = append(requests, query)
		w.Header().Set("Content-Type", "application/json")
		switch query.Get("Action") {
		case "DescribeDisks":
			_ = json.NewEncoder(w).Encode(map[string]any{"Disks": map[string]any{"Disk": []map[string]any{{"Type": "system", "Size": 2, "Category": "cloud_auto", "Status": "In_use"}}}})
		case "DescribeEipAddresses":
			_ = json.NewEncoder(w).Encode(map[string]any{"EipAddresses": map[string]any{"EipAddress": []map[string]any{{"AllocationId": "eip-1", "Status": "InUse", "Bandwidth": "200"}}}})
		default:
			t.Fatalf("unexpected action: %s", query.Get("Action"))
		}
	}))
	defer server.Close()

	client := &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2014-05-26", Product: "Ecs", AccessKey: "ak", Secret: "sk"}
	resources, err := (&Service{ECS: client, EIP: client}).DescribeBillingResources(context.Background(), "cn-hongkong", []string{"i-1"})
	if err != nil {
		t.Fatal(err)
	}
	instance := resources["i-1"]
	if instance.SystemDisk == nil || instance.SystemDisk.Size != 2 || instance.SystemDisk.Category != "cloud_auto" {
		t.Fatalf("unexpected system disk: %#v", instance)
	}
	eip := resources["eip-1"]
	if eip.EIP == nil || eip.EIP.Count != 1 || eip.EIP.Bandwidth != 200 || eip.EIP.Status != "InUse" {
		t.Fatalf("unexpected eip: %#v", eip)
	}
	if len(requests) != 2 || requests[0].Get("InstanceId") != "i-1" || requests[1].Get("InstanceId") != "i-1" || requests[1].Get("InstanceType") != "EcsInstance" {
		t.Fatalf("unexpected resource lookup requests: %#v", requests)
	}
}

func TestDescribeInstancePublicNetworksMapsEIPBandwidth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") != "DescribeEipAddresses" || r.URL.Query().Get("InstanceId") != "i-1" {
			t.Fatalf("unexpected EIP request: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"EipAddresses": map[string]any{"EipAddress": []map[string]any{{"AllocationId": "eip-1", "IpAddress": "203.0.113.20", "Bandwidth": "200"}}}})
	}))
	defer server.Close()

	client := &RPCClient{HTTPClient: server.Client(), Endpoint: server.URL, Version: "2016-04-28", Product: "Vpc", AccessKey: "ak", Secret: "sk"}
	networks, err := (&Service{EIP: client}).DescribeInstancePublicNetworks(context.Background(), "cn-hongkong", []string{"i-1"})
	if err != nil {
		t.Fatal(err)
	}
	network, ok := networks["i-1"]
	if !ok || network.AllocationID != "eip-1" || network.Address != "203.0.113.20" || network.Bandwidth != 200 {
		t.Fatalf("unexpected network: %#v", networks)
	}
}
