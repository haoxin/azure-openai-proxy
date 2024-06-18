package azure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/stulzq/azure-openai-proxy/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

func ProxyWithConverter(requestConverter RequestConverter) gin.HandlerFunc {
	return func(c *gin.Context) {
		Proxy(c, requestConverter)
	}
}

type DeploymentInfo struct {
	Data   []map[string]interface{} `json:"data"`
	Object string                   `json:"object"`
}

func ModelProxy(c *gin.Context) {
	// Create a channel to receive the results of each request
	results := make(chan []map[string]interface{}, len(ModelDeploymentConfig))

	// Send a request for each deployment in the map
	for _, deployment := range ModelDeploymentConfig {
		go func(deployment DeploymentConfig) {
			// Create the request
			req, err := http.NewRequest(http.MethodGet, deployment.Endpoint+"/openai/deployments?api-version=2022-12-01", nil)
			if err != nil {
				log.Printf("error parsing response body for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}

			// Set the auth header
			req.Header.Set(AuthHeaderKey, deployment.ApiKey)

			// Send the request
			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("error sending request for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Printf("unexpected status code %d for deployment %s", resp.StatusCode, deployment.DeploymentName)
				results <- nil
				return
			}

			// Read the response body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("error reading response body for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}

			// Parse the response body as JSON
			var deplotmentInfo DeploymentInfo
			err = json.Unmarshal(body, &deplotmentInfo)
			if err != nil {
				log.Printf("error parsing response body for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}
			results <- deplotmentInfo.Data
		}(deployment)
	}

	// Wait for all requests to finish and collect the results
	var allResults []map[string]interface{}
	for i := 0; i < len(ModelDeploymentConfig); i++ {
		result := <-results
		if result != nil {
			allResults = append(allResults, result...)
		}
	}
	var info = DeploymentInfo{Data: allResults, Object: "list"}
	combinedResults, err := json.Marshal(info)
	if err != nil {
		log.Printf("error marshalling results: %v", err)
		util.SendError(c, err)
		return
	}

	// Set the response headers and body
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, string(combinedResults))
}

// Proxy Azure OpenAI
func Proxy(c *gin.Context, requestConverter RequestConverter) {
	if c.Request.Method == http.MethodOptions {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, OPTIONS, POST")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Status(200)
		return
	}

	// preserve request body for error logging
	var buf bytes.Buffer
	tee := io.TeeReader(c.Request.Body, &buf)
	bodyBytes, err := io.ReadAll(tee)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		return
	}
	c.Request.Body = io.NopCloser(&buf)

	director := func(req *http.Request) {
		/*
			Step 2: Create a thread, Body is null
			Step 7: List all the Assistants, Body is null
			Step 5: Check the run status, Body is null
			Step 6: Display the Assistant response, Body is null
			Step 3: Create a message, Body is not null, but no model in the body
			Step 4: Run the Assistant, Body is not null, but no model in the body
			Step 1: Create an Assistant, Body is not null, and model in the body!!!

		*/
		if req.Body == nil {
			//log.Printf("request body is empty,treat it as Assistants request")
			SetHeader(c, req, "")
			ConvertURL(c, req, "assistants", requestConverter)
			return
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(body))
		// get model from url params or body
		model := GetModelName(c, body, req)
		SetHeader(c, req, model)
		ConvertURL(c, req, model, requestConverter)
	}

	proxy := &httputil.ReverseProxy{Director: director}
	transport, err := util.NewProxyFromEnv()
	if err != nil {
		util.SendError(c, errors.Wrap(err, "azure-openai-proxy: get proxy error"))
		return
	}
	if transport != nil {
		proxy.Transport = transport
	}

	proxy.ServeHTTP(c.Writer, c.Request)

	// issue: https://github.com/Chanzhaoyu/chatgpt-web/issues/831
	if c.Writer.Header().Get("Content-Type") == "text/event-stream" {
		if _, err := c.Writer.Write([]byte{'\n'}); err != nil {
			log.Printf("rewrite response error: %v", err)
		}
	}

	if c.Writer.Status() != 200 {
		if bodyBytes != nil && len(bodyBytes) > 0 {
			log.Printf("encountering error with body: %s", string(bodyBytes))
		}
	}
}

func GetDeploymentByModel(model string) (*DeploymentConfig, error) {
	deploymentConfig, exist := ModelDeploymentConfig[model]
	if !exist {
		return nil, errors.New(fmt.Sprintf("deployment config for %s not found", model))
	}
	return &deploymentConfig, nil
}
func SetHeader(c *gin.Context, req *http.Request, model string) {
	//为了处理assistants的请求，需要在header中加入apikey
	if model != "" && model == "assistants" {
		deployment, err := GetDeploymentByModel(model)
		if err != nil {
			util.SendError(c, errors.Wrap(err, "azure-openai-proxy: can't get assistants config, please add it in config.yaml"))
			return
		}
		req.Header.Set(AuthHeaderKey, deployment.ApiKey)
	} else {
		req.Header.Set(AuthHeaderKey, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		req.Header.Del("Authorization")
	}
}
func ConvertURL(c *gin.Context, req *http.Request, ModelName string, requestConverter RequestConverter) {
	originURL := req.URL.String()
	deployment, err := GetDeploymentByModel(ModelName)
	if err != nil {
		util.SendError(c, errors.Wrap(err, "azure-openai-proxy: can't get assistants config, please add it in config.yaml"))
		return
	}
	req, err = requestConverter.Convert(req, deployment)
	if err != nil {
		util.SendError(c, errors.Wrap(err, "azure-openai-proxy: convert request error"))
		return
	}
	log.Printf("proxying request:  %s -> %s", originURL, req.URL.String())
}

func GetModelName(c *gin.Context, body []byte, req *http.Request) string {
	if strings.Contains(req.URL.Path, "/assistants") || strings.Contains(req.URL.Path, "/threads") {
		return "assistants"
	}
	model := c.Param("model")
	if model == "" {
		_model, err := sonic.Get(body, "model")
		if err != nil {
			//log.Printf("can't get model from request body, treat it as Assistants requests")
			return "assistants"
		}
		_modelStr, err := _model.String()
		if err != nil {
			util.SendError(c, errors.Wrap(err, "azure-openai-proxy: get model name error"))
		}
		model = _modelStr
	}
	return model
}
