# 非同期ジョブ。進捗はここに書かれる。
class Job < ApplicationRecord
  belongs_to :document

  STATUS = { 1 => "待機", 2 => "処理中", 3 => "完了", 4 => "失敗" }.freeze
  STAGE  = {
    "preprocess" => "画像の補正", "ocr" => "文字の読み取り",
    "match" => "取引先の照合", "decide" => "判定"
  }.freeze

  def status_ja = STATUS[status] || "不明"
  def stage_ja  = STAGE[stage] || stage
  def running?  = status == 1 || status == 2
end
