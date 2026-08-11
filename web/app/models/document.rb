# 伝票1枚。
class Document < ApplicationRecord
  belongs_to :client
  belongs_to :uploaded_by, class_name: "User", optional: true
  has_many :document_pages, dependent: :destroy
  has_many :extracted_fields, dependent: :destroy
  has_many :match_candidates, dependent: :destroy
  has_one  :match_result, dependent: :destroy
  has_many :jobs, dependent: :destroy

  STATUS = {
    1 => "受付", 2 => "前処理済", 3 => "OCR済",
    4 => "照合済", 5 => "確定", 9 => "エラー"
  }.freeze
  DOC_TYPE = { 1 => "請求書", 2 => "納品書", 3 => "領収書" }.freeze

  def status_ja   = STATUS[status] || "不明"
  def doc_type_ja = DOC_TYPE[doc_type] || "不明"

  # 最新のジョブ。進捗はここから読む。ワーカーに問い合わせに行かない。
  def latest_job = jobs.order(id: :desc).first

  def field(key) = extracted_fields.find { |f| f.field_key == key }

  # 受領なら発行元、発行なら宛先が取引先。
  # 画面で「どちらを照合したか」が見えないと、
  # 名前が違うときに「読み間違い」か「向きの設定違い」かを分けられない。
  RECEIVED = 1
  ISSUED   = 2

  def direction_ja = direction == RECEIVED ? "受領" : "発行"

  def partner_field_key = direction == RECEIVED ? "issuer_name" : "recipient_name"

  def partner_field = field(partner_field_key) || field("partner_name")

  # 1枚の紙から切り出した伝票なら、元のどこだったかを示す。
  # これが無いと、レシート10枚が10件並んだときに
  # どれが手元のどの紙なのか突き合わせられない。
  def from_split? = source_name.present? && (source_page > 1 || source_region > 1)

  def source_ja
    return nil if source_name.blank?
    "#{source_name}　#{source_page}ページ目の#{source_region}件目"
  end

  # 同じファイルから受け付けた伝票。
  def siblings
    return Document.none if source_name.blank?
    Document.where(client_id: client_id, source_name: source_name)
            .where.not(id: id).order(:source_page, :source_region)
  end
end
