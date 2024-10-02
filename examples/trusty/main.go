package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"fmt"
	"log"
	"time"

	"github.com/stackloklabs/gollm/pkg/backend"
	"github.com/stackloklabs/gollm/pkg/config"
)

var trustyTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "trustyReport",
		"description": "Evaluate the trustworthiness of a package",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"package_name": map[string]any{
					"type":        "string",
					"description": "The name of the package",
				},
				"ecosystem": map[string]any{
					"type":        "string",
					"description": "The ecosystem of the package",
				},
			},
			"required": []string{"package_name", "ecosystem"},
		},
	},
}

func trustyReport(packageName string, ecosystem string) (string, error) {
	// Build the URL with package name and type
	url := fmt.Sprintf("https://api.trustypkg.dev/v1/report?package_name=%s&package_type=%s", packageName, ecosystem)

	// Create a new HTTP request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	// Set the header to accept JSON
	req.Header.Set("accept", "application/json")

	// Perform the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Convert body to a JSON string
	var prettyJSON map[string]interface{}
	err = json.Unmarshal(body, &prettyJSON)
	if err != nil {
		return "", err
	}

	// Convert the JSON back to string
	jsonString, err := json.MarshalIndent(prettyJSON, "", "  ")
	if err != nil {
		return "", err
	}

	return string(jsonString), nil
}

func responseAsMap(resp backend.OllamaResponseMessage) map[string]any {
	data, err := json.Marshal(resp)
	if err != nil {
		return nil
	}

	var result map[string]any
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil
	}

	return result
}

func chatTrustyReport(ctx context.Context, ollBe *backend.OllamaBackend, messages []map[string]any, call backend.OllamaFunctionCall) (backend.OllamaResponseMessage, error) {
	pkgName, ok := call.Arguments["package_name"].(string)
	if !ok {
		log.Fatalf("failed to get package name from Ollama response")
	}
	ecoSystem, ok := call.Arguments["ecosystem"].(string)
	if !ok {
		log.Fatalf("failed to get ecosystem from Ollama response")
	}

	trustyReply, err := trustyReport(pkgName, ecoSystem)
	if err != nil {
		log.Fatalf("failed to get trusty report: %v", err)
	}

	messages = append(messages, map[string]any{
		"role":    "tool",
		"content": trustyReply,
	})

	ollamaResponse, err := ollBe.Chat(ctx, messages, nil)
	if err != nil {
		if err == context.DeadlineExceeded {
			log.Fatal("timeout while waiting for Ollama response")
		}
		log.Fatalf("failed to generate response: %v", err)
	}

	return ollamaResponse.Message, nil
}

func main() {
	cfg := config.InitializeViperConfig("config", "yaml", ".")

	// OLLAMA Example
	ollamaBackend := backend.NewOllamaBackend(cfg.Get("ollama.host"), cfg.Get("ollama.model"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userPrompt := os.Args[1:]
	promptBase := ` You are to help a user with selecting a dependency.
Your job is to provide a recommendation based on the user's prompt. 
You might be provided a JSON summary along with the user's prompt. Do not summarize the JSON back to the user.
Focus on whether the package is malicious or deprecated based on the provided tool input you get as JSON.
Focus less on the number of stars or forks.
If the package is malicious or deprecated, recommend a safer alternative.
If the package is safe, recommend the package.
Provide a short summary, do not summarize the tool output.
The user says: `
	prompt := promptBase + " " + strings.Join(userPrompt, " ")
	messages := []map[string]any{
		{
			"role":    "user",
			"content": prompt,
		},
	}
	tools := []map[string]any{
		trustyTool,
	}
	ollamaResponse, err := ollamaBackend.Chat(ctx, messages, tools)
	if err != nil {
		if err == context.DeadlineExceeded {
			log.Fatal("timeout while waiting for Ollama response")
		}
		log.Fatalf("failed to generate response: %v", err)
	}

	// keep the message in the array of messages we send to keep the context going
	messages = append(messages, responseAsMap(ollamaResponse.Message))

	if len(ollamaResponse.Message.ToolCalls) == 0 {
		fmt.Println("The model did not use tools. It generated the response on its own.")
		fmt.Println("Content: ", ollamaResponse.Message.Content)
		return
	}

	// TODO: handle multiple tool calls
	funcName := ollamaResponse.Message.ToolCalls[0].Function.Name
	if funcName == "trustyReport" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		log.Println("Handling trustyReport function call")
		ollamaTrustyMessage, err := chatTrustyReport(ctx, ollamaBackend, messages, ollamaResponse.Message.ToolCalls[0].Function)
		if err != nil {
			log.Fatalf("failed to generate response: %v", err)
		}

		log.Println("Handling trustyReport response")

		messages := []map[string]any{
			{
				"role":    "user",
				"content": ollamaTrustyMessage.Content,
			},
			{
				"role": "user",
				"content": `Summarize the previous response in a single short paragraph. Focus on whether the package is
malicious or deprecated. If you advise to not use the package, recommend a safer alternative. If the package is safe, recommend the package.`,
			},
		}

		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ollamaResponse, err = ollamaBackend.Chat(ctx, messages, nil)
		if err != nil {
			if err == context.DeadlineExceeded {
				log.Fatal("timeout while waiting for Ollama response")
			}
			log.Fatalf("failed to generate response: %v", err)
		}

	} else {
		log.Fatalf("unexpected function call: %s", funcName)
	}

	fmt.Printf("Response:\n%s\n", ollamaResponse.Message.Content)
}
