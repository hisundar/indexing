package secondaryindex

import (
	"bytes"
	"encoding/json"
	"errors"
	c "github.com/couchbase/indexing/secondary/common"
	tc "github.com/couchbase/indexing/secondary/tests/framework/common"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
)

type IndexProperties struct {
	HostNode      string
	HttpPort      string
	Bucket        string
	IndexFilePath string
}

func GetIndexerNodesHttpAddresses(hostaddress string) ([]string, error) {
	clusterURL, err := c.ClusterAuthUrl(hostaddress)
	if err != nil {
		return nil, err
	}

	cinfo, err := c.NewClusterInfoCache(clusterURL, "default")
	if err != nil {
		return nil, err
	}

	if err := cinfo.Fetch(); err != nil {
		return nil, err
	}

	node_ids := cinfo.GetNodesByServiceType(c.INDEX_HTTP_SERVICE)
	indexNodes := []string{}
	for _, node_id := range node_ids {
		addr, _ := cinfo.GetServiceAddress(node_id, c.INDEX_HTTP_SERVICE)
		indexNodes = append(indexNodes, addr)
	}

	return indexNodes, nil
}

func GetStatsForIndexerHttpAddress(indexerHttpAddr, serverUserName, serverPassword string) map[string]interface{} {
	client := &http.Client{}
	address := "http://" + indexerHttpAddr + "/stats"

	req, _ := http.NewRequest("GET", address, nil)
	req.SetBasicAuth(serverUserName, serverPassword)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	resp, err := client.Do(req)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		log.Printf(address)
		log.Printf("%v", req)
		log.Printf("%v", resp)
		log.Printf("Get stats failed\n")
	}
	// todo : error out if response is error
	tc.HandleError(err, "Get Stats")
	defer resp.Body.Close()

	response := make(map[string]interface{})
	body, _ := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(body, &response)
	if err != nil {
		tc.HandleError(err, "Get Bucket :: Unmarshal of response body")
	}

	return response
}

func GetIndexStats(indexName, bucketName, serverUserName, serverPassword, hostaddress string) map[string]interface{} {
	indexNodes, _ := GetIndexerNodesHttpAddresses(hostaddress)
	indexStats := make(map[string]interface{})

	for _, indexNode := range indexNodes {
		stats := GetStatsForIndexerHttpAddress(indexNode, serverUserName, serverPassword)
		for statKey := range stats {
			if strings.Contains(statKey, bucketName+":"+indexName) {
				indexStats[statKey] = stats[statKey]
			}
		}
	}
	return indexStats
}

func ChangeIndexerSettings(configKey string, configValue interface{}, serverUserName, serverPassword, hostaddress string) error {
	log.Printf("DBG: ChangeIndexerSettings: configKey = %v configValue = %v hostaddress = %v", configKey, configValue, hostaddress)
	qpclient, err := CreateClient(hostaddress, "2i_settings")
	defer qpclient.Close()
	if err != nil {
		return err
	}
	nodes, err := qpclient.Nodes()
	log.Printf("DBG: ChangeIndexerSettings: nodes = %v", nodes)
	if err != nil {
		return err
	}

	var adminurl string
	for _, indexer := range nodes {
		adminurl = indexer.Adminport
		break
	}

	host, sport, _ := net.SplitHostPort(adminurl)
	log.Printf("DBG: ChangeIndexerSettings: adminurl = %v host %v sport %v", adminurl, host, sport)
	iport, _ := strconv.Atoi(sport)

	if host == "" || iport == 0 {
		log.Printf("DBG: ChangeIndexerSettings: Host %v Port %v Nodes %+v", host, iport, nodes)
	}

	client := http.Client{}
	// hack, fix this
	ihttp := iport + 2
	url := "http://" + host + ":" + strconv.Itoa(ihttp) + "/internal/settings"

	if len(configKey) > 0 {
		log.Printf("Changing config key %v to value %v\n", configKey, configValue)
		jbody := make(map[string]interface{})
		jbody[configKey] = configValue
		pbody, err := json.Marshal(jbody)
		if err != nil {
			return err
		}
		preq, err := http.NewRequest("POST", url, bytes.NewBuffer(pbody))
		preq.SetBasicAuth(serverUserName, serverPassword)

		_, err = client.Do(preq)
		if err != nil {
			return err
		}
	}

	return nil
}

func GetIndexHostNode(indexName, bucketName, serverUserName, serverPassword, hostaddress string) (string, error) {
	client := &http.Client{}
	address := "http://" + hostaddress + "/indexStatus"

	req, _ := http.NewRequest("GET", address, nil)
	req.SetBasicAuth(serverUserName, serverPassword)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	resp, err := client.Do(req)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		log.Printf(address)
		log.Printf("%v", req)
		log.Printf("%v", resp)
		log.Printf("Get indexStatus failed")
	}
	// todo : error out if response is error
	tc.HandleError(err, "Get Stats")
	defer resp.Body.Close()

	response := make(map[string]interface{})
	body, _ := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(body, &response)

	if err != nil {
		tc.HandleError(err, "Get IndexStatus :: Unmarshal of response body")
		return "", nil
	}

	c, e := CreateClient(hostaddress, "2itest")
	if e != nil {
		return "", e
	}
	defer c.Close()

	defnID, _ := GetDefnID(c, bucketName, indexName)

	indexes := response["indexes"].([]interface{})
	for _, index := range indexes {
		i := index.(map[string]interface{})
		if i["id"].(float64) == float64(defnID) {
			hosts := i["hosts"].([]interface{})
			return hosts[0].(string), nil
		}
	}

	return "", errors.New("Index not found in /indexStatus")
}

func GetIndexHttpPort(indexHostAddress, serverUserName, serverPassword, hostaddress string) string {
	client := &http.Client{}
	address := "http://" + hostaddress + "/pools/default/nodeServices"

	req, _ := http.NewRequest("GET", address, nil)
	req.SetBasicAuth(serverUserName, serverPassword)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	resp, err := client.Do(req)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		log.Printf(address)
		log.Printf("%v", req)
		log.Printf("%v", resp)
		log.Printf("Get indexStatus failed")
	}
	// todo : error out if response is error
	tc.HandleError(err, "Get Stats")
	defer resp.Body.Close()

	response := make(map[string]interface{})
	body, _ := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(body, &response)

	if err != nil {
		tc.HandleError(err, "Get nodeServices :: Unmarshal of response body")
		return ""
	}

	log.Printf("%v", response)
	return ""
}
