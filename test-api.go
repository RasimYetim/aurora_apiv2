package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"github.com/joho/godotenv"
)

func main() {
	// .env dosyasını oku
	_ = godotenv.Load()
	apiKey := os.Getenv("GEMINI_API_KEY")

	if apiKey == "" {
		log.Fatal("GEMINI_API_KEY bulunamadı")
	}

	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	fmt.Println("API Anahtarınızın Erişebildiği Modeller:")
	fmt.Println("----------------------------------------")

	// ModelService.ListModels çağrısı
	iter := client.ListModels(ctx)
	for {
		m, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatalf("Modeller listelenirken hata: %v", err)
		}
		fmt.Printf("- Model Adı: %s\n", m.Name)
		fmt.Printf("  Açıklama: %s\n", m.Description)
		fmt.Printf("  Desteklenen Metodlar: %v\n\n", m.SupportedGenerationMethods)
	}
}
