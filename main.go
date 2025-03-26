package main

import (
  "bytes"
  "crypto/tls"
  "encoding/json"
  "fmt"
  "io"
  "log"
  "net/http"
  "os"

  "github.com/aws/aws-sdk-go/aws"
  "github.com/aws/aws-sdk-go/aws/credentials"
  "github.com/aws/aws-sdk-go/aws/session"
  "github.com/aws/aws-sdk-go/service/s3"
  "github.com/gin-gonic/gin"
  "github.com/gin-contrib/cors"
  "github.com/go-redis/redis/v8"
  "golang.org/x/net/context"
  "gorm.io/driver/mysql"
  "gorm.io/gorm"
)

// Student Model
type Student struct {
  ID     uint   `json:"id" gorm:"primaryKey"`
  Name   string `json:"name"`
  School string `json:"school"`
  Photo  string `json:"photo"`
}

// Global Variables
var (
  db       *gorm.DB
  rdb      *redis.Client
  ctx      = context.Background()
  s3Client *s3.S3
)

// Initialize MariaDB Connection
func initDB() {
  dbUser := os.Getenv("DB_USER")
  dbPass := os.Getenv("DB_PASS")
  dbHost := os.Getenv("DB_HOST")
  dbPort := os.Getenv("DB_PORT")
  dbName := os.Getenv("DB_NAME")

  dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
    dbUser, dbPass, dbHost, dbPort, dbName)

  var err error
  db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
  if err != nil {
    log.Fatal("Failed to connect to database:", err)
  }

  db.AutoMigrate(&Student{})
}

// Initialize Redis Connection with TLS
func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:      os.Getenv("REDIS_ADDR"), // Example: "your-redis-endpoint.cache.amazonaws.com:6379"
		Password:  "",                      // Set if needed
		DB:        0,
		TLSConfig: &tls.Config{},           // Enable TLS
	})

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatal("Failed to connect to Redis:", err)
	} else {
		log.Println("Connected to Redis with TLS successfully!")
	}
}

// Initialize AWS S3
func initS3() {
  sess, err := session.NewSession(&aws.Config{
    Region: aws.String(os.Getenv("AWS_REGION")),
    Credentials: credentials.NewStaticCredentials(
      os.Getenv("AWS_ACCESS_KEY"),
      os.Getenv("AWS_SECRET_KEY"),
      "",
    ),
  })
  if err != nil {
    log.Fatal("Failed to initialize AWS S3 session:", err)
  }

  s3Client = s3.New(sess)
}

// Upload Photo to AWS S3
func uploadPhoto(file io.Reader, fileName string) (string, error) {
    bucket := os.Getenv("AWS_BUCKET_NAME")

    // Baca file ke dalam buffer agar bisa digunakan sebagai ReadSeeker
    buf, err := io.ReadAll(file)
    if err != nil {
        return "", err
    }
    fileReader := bytes.NewReader(buf)

    _, err = s3Client.PutObject(&s3.PutObjectInput{
        Bucket: aws.String(bucket),
        Key:    aws.String(fileName),
        Body:   fileReader, // Menggunakan bytes.Reader yang mengimplementasikan io.ReadSeeker
        ACL:    aws.String("public-read"),
    })
    if err != nil {
        return "", err
    }

    return fmt.Sprintf("https://%s.s3.amazonaws.com/%s", bucket, fileName), nil
}


// Create Student (with Photo Upload)
func createStudent(c *gin.Context) {
  name := c.PostForm("name")
  school := c.PostForm("school")
  file, header, err := c.Request.FormFile("photo")

  if err != nil {
    c.JSON(http.StatusBadRequest, gin.H{"error": "Photo is required"})
    return
  }
  defer file.Close()

  photoURL, err := uploadPhoto(file, header.Filename)
  if err != nil {
    log.Fatal("Failed to upload photo: ", err)
    c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to upload photo"})
    return
  }

  student := Student{Name: name, School: school, Photo: photoURL}
  db.Create(&student)

  // Store in Redis cache
  studentJSON, _ := json.Marshal(student)
  rdb.Set(ctx, fmt.Sprintf("student:%d", student.ID), studentJSON, 0)

  c.JSON(http.StatusOK, student)
}

// Get All Students
func getStudents(c *gin.Context) {
  var students []Student
  db.Find(&students)
  c.JSON(http.StatusOK, students)
}

// Get Student By ID
func getStudentByID(c *gin.Context) {
  id := c.Param("id")
  var student Student
  result := db.First(&student, id)

  if result.Error != nil {
    c.JSON(http.StatusNotFound, gin.H{"error": "Student not found"})
    return
  }

  c.JSON(http.StatusOK, student)
}

// Update Student
func updateStudent(c *gin.Context) {
  id := c.Param("id")
  var student Student
  result := db.First(&student, id)

  if result.Error != nil {
    c.JSON(http.StatusNotFound, gin.H{"error": "Student not found"})
    return
  }

  name := c.PostForm("name")
  school := c.PostForm("school")

  if name != "" {
    student.Name = name
  }
  if school != "" {
    student.School = school
  }

  db.Save(&student)

  // Update cache
  studentJSON, _ := json.Marshal(student)
  rdb.Set(ctx, fmt.Sprintf("student:%d", student.ID), studentJSON, 0)

  c.JSON(http.StatusOK, student)
}

// Delete Student
func deleteStudent(c *gin.Context) {
  id := c.Param("id")
  db.Delete(&Student{}, id)

  // Remove from Redis cache
  rdb.Del(ctx, fmt.Sprintf("student:%s", id))

  c.JSON(http.StatusOK, gin.H{"message": "Student deleted"})
}

// Get Student from Cache
func getStudentCache(c *gin.Context) {
  id := c.Param("id")
  data, err := rdb.Get(ctx, fmt.Sprintf("student:%s", id)).Result()

  if err != nil {
    c.JSON(http.StatusNotFound, gin.H{"error": "Student not found in cache"})
    return
  }

  c.String(http.StatusOK, data)
}

// Main function
func main() {
  initDB()
  initRedis()
  initS3()

  r := gin.Default()

  // Enable CORS Middleware
  r.Use(cors.New(cors.Config{
    AllowOrigins:     []string{"*"}, // Allow all origins (can be changed to specific domains)
    AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
    AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
    ExposeHeaders:    []string{"Content-Length"},
    AllowCredentials: true,
  }))
  
  r.POST("/students", createStudent)
  r.GET("/students", getStudents)
  r.GET("/students/:id", getStudentByID)
  r.PUT("/students/:id", updateStudent)
  r.DELETE("/students/:id", deleteStudent)
  r.GET("/students/cache/:id", getStudentCache)

  r.Run(":8080")
}
