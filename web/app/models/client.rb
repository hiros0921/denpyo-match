# 顧問先企業。伝票はこの単位で集まる。
class Client < ApplicationRecord
  belongs_to :organization
  has_many :partners
  has_many :documents

  # ローカル完結（tesseract）にできるのは顧問先単位。
  # 機密性を気にする顧問先だけ外部APIに出さない、という運用ができる。
  def engine_ja = ocr_engine == "tesseract" ? "ローカル(Tesseract)" : "Google Cloud Vision"
end
