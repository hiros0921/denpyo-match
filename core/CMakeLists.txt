# C++ コア。第3段階では前処理だけ。第4段階で照合エンジンを足す。
cmake_minimum_required(VERSION 3.20)
project(dm_core LANGUAGES CXX VERSION 0.1.0)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)          # GNU拡張を禁じ、規格通りに保つ

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

find_package(OpenCV REQUIRED)

# Tesseract は pkg-config で引く
find_package(PkgConfig REQUIRED)
pkg_check_modules(TESSERACT REQUIRED tesseract)
pkg_check_modules(LEPTONICA REQUIRED lept)

add_library(dm_preprocess_lib
  src/preprocess/pdf.cpp
  src/preprocess/split.cpp
  src/preprocess/preprocess.cpp
)
target_include_directories(dm_preprocess_lib PUBLIC include ${OpenCV_INCLUDE_DIRS})
target_link_libraries(dm_preprocess_lib PUBLIC ${OpenCV_LIBS})
target_compile_options(dm_preprocess_lib PRIVATE
  -Wall -Wextra -Wpedantic -Wshadow)

# 正規化。照合エンジンとマスタ登録の両方から使うので、独立したライブラリにする。
add_library(dm_common_lib src/common/normalize.cpp)
target_include_directories(dm_common_lib PUBLIC include)
target_compile_options(dm_common_lib PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

# CLI。単体で動くことが仕様書の要件。
add_executable(dm_preprocess src/preprocess/main.cpp)
target_link_libraries(dm_preprocess PRIVATE dm_preprocess_lib)
target_compile_options(dm_preprocess PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

message(STATUS "OpenCV ${OpenCV_VERSION}")

add_executable(dm_normalize src/common/main.cpp src/common/normalize_test.cpp)
target_link_libraries(dm_normalize PRIVATE dm_common_lib)
target_compile_options(dm_normalize PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

# 照合エンジン。C++ を使う必然性が最も明確に出る部分。
add_library(dm_matching_lib src/matching/matching.cpp)
target_include_directories(dm_matching_lib PUBLIC include)
target_link_libraries(dm_matching_lib PUBLIC dm_common_lib)
target_compile_options(dm_matching_lib PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

add_executable(dm_match src/matching/main.cpp src/matching/matching_test.cpp)
target_link_libraries(dm_match PRIVATE dm_matching_lib)
target_compile_options(dm_match PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

# OCR。エンジンを差し替え可能にする。実装は2つ用意して、それを証明する。
add_library(dm_ocr_lib
  src/ocr/tesseract_engine.cpp
  src/ocr/extract.cpp
)
target_include_directories(dm_ocr_lib PUBLIC include ${TESSERACT_INCLUDE_DIRS} ${LEPTONICA_INCLUDE_DIRS})
target_link_libraries(dm_ocr_lib PUBLIC ${TESSERACT_LIBRARIES} ${LEPTONICA_LIBRARIES})
target_compile_options(dm_ocr_lib PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

add_executable(dm_ocr src/ocr/main.cpp)
target_link_libraries(dm_ocr PRIVATE dm_ocr_lib)
target_compile_options(dm_ocr PRIVATE -Wall -Wextra -Wpedantic -Wshadow)
